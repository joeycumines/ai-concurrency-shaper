package transcode

import (
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// emitTextDelta opens the message item and content part on first text and
// emits one response.output_text.delta per chunk.
func (s *geminiResponsesStreamState) emitTextDelta(delta string) ([]ResponsesSSEEvent, error) {
	if delta == "" {
		return nil, nil
	}
	// The bounds are checked before the item and part are opened, so a delta
	// that cannot fit leaves no output behind.
	if err := s.checkDeltaBounds(len(delta), "text delta"); err != nil {
		return nil, err
	}
	events, err := s.ensureMessageOpen()
	if err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	message := item.item.(*ResponsesOutputMessage)
	var contentIndex int64
	var key geminiPartKey
	if item.openPartIndex >= 0 && len(message.Content) > int(item.openPartIndex) {
		// Continue the open content part: Gemini streams one text part as
		// several chunks.
		contentIndex = item.openPartIndex
		key = geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
	} else {
		if err := s.budget.addPart(); err != nil {
			return nil, s.wireError(err)
		}
		contentIndex = int64(len(message.Content))
		key = geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
		s.textBufs[key] = &strings.Builder{}
		message.Content = append(message.Content, &ResponsesOutputText{
			Type:        "output_text",
			Text:        "",
			Annotations: []ResponsesAnnotation{},
		})
		item.openPartIndex = contentIndex
		events = append(events, s.builder.ContentPartAdded(
			message.ID,
			item.outputIndex,
			contentIndex,
			&ResponsesStreamOutputTextPart{
				Type:        "output_text",
				Text:        "",
				Annotations: []ResponsesAnnotation{},
			},
		))
	}
	buf := s.textBufs[key]
	if err := s.checkAccumulated(buf, len(delta), "text delta"); err != nil {
		return nil, err
	}
	buf.WriteString(delta)
	events = append(events, s.builder.TextDelta(
		message.ID, item.outputIndex, contentIndex, delta,
	))
	return events, nil
}

// emitThoughtText renders thought text as a reasoning summary delta when the
// capability maps it to thinking; the summary is a reasoning item.
func (s *geminiResponsesStreamState) emitThoughtText(delta, path string) ([]ResponsesSSEEvent, error) {
	if delta == "" {
		return nil, nil
	}
	_ = path
	// The bounds are checked before the reasoning item is opened, so a delta
	// that cannot fit leaves no output behind.
	if err := s.checkDeltaBounds(len(delta), "thought delta"); err != nil {
		return nil, err
	}
	events, err := s.ensureReasoningOpen()
	if err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	reasoning := item.item.(*ResponsesReasoningOutputItem)
	buf := s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}]
	if buf == nil {
		buf = &strings.Builder{}
		s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}] = buf
		if len(reasoning.Summary) == 0 {
			if err := s.budget.addPart(); err != nil {
				return nil, s.wireError(err)
			}
			reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: ""}}
			events = append(events, s.builder.ReasoningSummaryPartAdded(
				reasoning.ID, item.outputIndex, 0, openairesponses.SummaryTextPart{
					Type: "summary_text", Text: "",
				},
			))
		}
	}
	if err := s.checkAccumulated(buf, len(delta), "thought delta"); err != nil {
		return nil, err
	}
	buf.WriteString(delta)
	events = append(events, s.builder.ReasoningSummaryTextDelta(
		reasoning.ID, item.outputIndex, 0, delta,
	))
	return events, nil
}

// ensureMessageOpen opens a new output message item when the last item is not
// an open message.
func (s *geminiResponsesStreamState) ensureMessageOpen() ([]ResponsesSSEEvent, error) {
	events := s.closeOpenReasoning()
	if n := len(s.items); n > 0 {
		if item := &s.items[n-1]; item.isMessage() && item.item.(*ResponsesOutputMessage).Status != ResponsesItemCompleted {
			return events, nil
		}
	}
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	message := &ResponsesOutputMessage{
		ID:      s.ctx.IDs.New("msg_"),
		Type:    "message",
		Role:    "assistant",
		Status:  ResponsesItemInProgress,
		Content: []ResponsesOutputContentPart{},
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: message, openPartIndex: -1})
	// The added event carries a detached snapshot of the item at creation:
	// the live item gains parts and is completed later, possibly within the
	// same chunk, before the event is marshaled.
	snapshot := *message
	snapshot.Content = ResponsesOutputContentParts{}
	return append(events, s.builder.OutputItemAdded(outputIndex, &snapshot)), nil
}

// ensureReasoningOpen opens a reasoning item when none is open.
func (s *geminiResponsesStreamState) ensureReasoningOpen() ([]ResponsesSSEEvent, error) {
	if n := len(s.items); n > 0 {
		if item := &s.items[n-1]; isGeminiReasoningItem(item) &&
			item.item.(*ResponsesReasoningOutputItem).Status != ResponsesItemCompleted {
			return nil, nil
		}
	}
	events := s.closeOpenMessage()
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	reasoning := &ResponsesReasoningOutputItem{
		ID:      s.ctx.IDs.New("rs_"),
		Type:    "reasoning",
		Status:  ResponsesItemInProgress,
		Summary: []ResponsesReasoningSummary{},
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: reasoning, openPartIndex: -1})
	// A detached creation snapshot, as for message items.
	snapshot := *reasoning
	snapshot.Summary = []ResponsesReasoningSummary{}
	return append(events, s.builder.OutputItemAdded(outputIndex, &snapshot)), nil
}

// closeOpenPart emits the done events of the message item's open content part
// (text or refusal done, then content_part.done), materializing the streamed
// text into the part. It does nothing when no part is open.
func (s *geminiResponsesStreamState) closeOpenPart(item *openResponsesItem) []ResponsesSSEEvent {
	message, ok := item.item.(*ResponsesOutputMessage)
	if !ok || item.openPartIndex < 0 || int(item.openPartIndex) >= len(message.Content) {
		item.openPartIndex = -1
		return nil
	}
	contentIndex := item.openPartIndex
	item.openPartIndex = -1
	switch part := message.Content[contentIndex].(type) {
	case *ResponsesOutputText:
		if buf := s.textBufs[geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}]; buf != nil {
			part.Text = buf.String()
		}
		return []ResponsesSSEEvent{
			s.builder.TextDone(message.ID, item.outputIndex, contentIndex, part.Text),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamOutputTextPart{
					Type:        "output_text",
					Text:        part.Text,
					Annotations: []ResponsesAnnotation{},
				}),
		}
	case *ResponsesOutputRefusal:
		return []ResponsesSSEEvent{
			s.builder.RefusalDone(message.ID, item.outputIndex, contentIndex, part.Refusal),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamRefusalPart{Type: "refusal", Refusal: part.Refusal}),
		}
	}
	return nil
}

// closeOpenMessage closes the last item when it is an open message: its open
// content part's done events, then output_item.done with status completed.
func (s *geminiResponsesStreamState) closeOpenMessage() []ResponsesSSEEvent {
	n := len(s.items)
	if n == 0 {
		return nil
	}
	item := &s.items[n-1]
	message, ok := item.item.(*ResponsesOutputMessage)
	if !ok || message.Status == ResponsesItemCompleted {
		return nil
	}
	events := s.closeOpenPart(item)
	message.Status = ResponsesItemCompleted
	return append(events, s.builder.OutputItemDone(item.outputIndex, message))
}

// closeOpenReasoning closes the last item when it is an open reasoning item:
// the summary text and part done events, then output_item.done with status
// completed.
func (s *geminiResponsesStreamState) closeOpenReasoning() []ResponsesSSEEvent {
	n := len(s.items)
	if n == 0 {
		return nil
	}
	item := &s.items[n-1]
	reasoning, ok := item.item.(*ResponsesReasoningOutputItem)
	if !ok || reasoning.Status == ResponsesItemCompleted {
		return nil
	}
	text := ""
	if buf := s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}]; buf != nil {
		text = buf.String()
	}
	if len(reasoning.Summary) == 0 {
		reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: text}}
	} else {
		reasoning.Summary[0].Text = text
	}
	reasoning.Status = ResponsesItemCompleted
	return []ResponsesSSEEvent{
		s.builder.ReasoningSummaryTextDone(reasoning.ID, item.outputIndex, 0, text),
		s.builder.ReasoningSummaryPartDone(reasoning.ID, item.outputIndex, 0, openairesponses.SummaryTextPart{
			Type: "summary_text", Text: text,
		}),
		s.builder.OutputItemDone(item.outputIndex, reasoning),
	}
}

// closeAllAndHold closes the open item and returns its closing events, which
// the caller holds with the terminal: the terminal envelope is appended at
// release so a trailing usage chunk is reflected. Function-call items are
// complete when emitted, and every earlier item was closed when the next one
// opened, so only the last item can still be open.
func (s *geminiResponsesStreamState) closeAllAndHold() []ResponsesSSEEvent {
	return append(s.closeOpenMessage(), s.closeOpenReasoning()...)
}
