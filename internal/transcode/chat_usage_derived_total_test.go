package transcode

// Tests for deriving a single missing usage total on a chat stream tail.
//
// The pinned CompletionUsage requires prompt/completion/total together, but an
// honest upstream tail can omit exactly one. A single missing total is derived
// from the two present values (never defaulted to zero) and the derivation is
// recorded as the usage_total_derived note; two or more missing totals cannot
// be derived and remain a typed upstream wire error. A subtraction that cannot
// yield a non-negative component is absorbed too - the source's arithmetic is
// never an exchange failure - and the derived component is clamped to zero,
// the only defensible count, never copied from the sibling operand.

import (
	"errors"
	"strings"
	"testing"
)

// usageTail builds a usage-only chat stream chunk with the given totals; an
// empty string means that key is omitted.
func usageTail(prompt, completion, total string) string {
	parts := make([]string, 0, 3)
	if prompt != "" {
		parts = append(parts, `"prompt_tokens":`+prompt)
	}
	if completion != "" {
		parts = append(parts, `"completion_tokens":`+completion)
	}
	if total != "" {
		parts = append(parts, `"total_tokens":`+total)
	}
	return `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{` +
		strings.Join(parts, ",") + `}}`
}

// TestChatStreamDerivesSingleMissingTotal proves each single-missing-total
// shape is derived arithmetically and marked with the derived key.
func TestChatStreamDerivesSingleMissingTotal(t *testing.T) {
	cases := map[string]struct {
		body    string
		derived string
		prompt  int
		comple  int
		total   int
	}{
		"missing_total":      {usageTail("5", "3", ""), "total_tokens", 5, 3, 8},
		"missing_completion": {usageTail("5", "", "8"), "completion_tokens", 5, 3, 8},
		"missing_prompt":     {usageTail("", "3", "8"), "prompt_tokens", 5, 3, 8},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(c.body)})
			if err != nil {
				t.Fatalf("single missing total rejected: %v", err)
			}
			if chunk.DerivedUsageTotal != c.derived {
				t.Fatalf("DerivedUsageTotal = %q, want %q", chunk.DerivedUsageTotal, c.derived)
			}
			if chunk.Usage == nil {
				t.Fatal("usage is nil")
			}
			if chunk.Usage.PromptTokens != c.prompt ||
				chunk.Usage.CompletionTokens != c.comple ||
				chunk.Usage.TotalTokens != c.total {
				t.Fatalf(
					"usage = prompt %d completion %d total %d, want %d/%d/%d",
					chunk.Usage.PromptTokens,
					chunk.Usage.CompletionTokens,
					chunk.Usage.TotalTokens,
					c.prompt,
					c.comple,
					c.total,
				)
			}
		})
	}
}

// TestChatStreamTwoMissingTotalsRejected proves a tail missing two or more
// totals cannot be derived and stays a typed upstream wire error.
func TestChatStreamTwoMissingTotalsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"only_total":      usageTail("", "", "8"),
		"only_prompt":     usageTail("5", "", ""),
		"only_completion": usageTail("", "3", ""),
		"none":            usageTail("", "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)})
			if err == nil {
				t.Fatal("two missing totals accepted")
			}
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
			}
		})
	}
}

// TestChatStreamImpossibleDerivationIsNotRejected proves a subtraction that
// cannot yield a non-negative component does not fail the exchange. The source
// reporting an inconsistent triple is a provider quirk, absorbed and recorded -
// the same treatment the all-present case gets, and the same treatment the
// non-streaming renderer gives it.
//
// The DERIVED component is the load-bearing assertion: the source's total is
// smaller than the component it is supposed to contain, so no non-negative
// component exists. Zero is the only defensible count, and the sibling totals
// are the ones the source actually sent. Copying the other operand (or
// subtracting to a negative and forwarding it) would report a number derived
// from nothing, so the emitted triple is checked value by value.
func TestChatStreamImpossibleDerivationIsNotRejected(t *testing.T) {
	cases := map[string]struct {
		body    string
		derived string
		prompt  int
		comple  int
		total   int
	}{
		// completion = total - prompt = 8 - 9, which is negative: clamp to 0
		// and relay the two counts the source did report.
		"total_lt_prompt": {usageTail("9", "", "8"), "completion_tokens", 9, 0, 8},
		// prompt = total - completion = 8 - 9, likewise.
		"total_lt_completion": {usageTail("", "9", "8"), "prompt_tokens", 0, 9, 8},
		// The boundary: total == prompt leaves exactly zero, which is a real
		// count and must be derived, not clamped away or rejected.
		"total_eq_prompt": {usageTail("8", "", "8"), "completion_tokens", 8, 0, 8},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(c.body)})
			if err != nil {
				t.Fatalf("a source-side inconsistency must be absorbed, not rejected: %v", err)
			}
			if chunk.DerivedUsageTotal != c.derived {
				t.Fatalf("DerivedUsageTotal = %q, want %q", chunk.DerivedUsageTotal, c.derived)
			}
			if chunk.Usage == nil {
				t.Fatal("usage is nil")
			}
			if chunk.Usage.PromptTokens != c.prompt ||
				chunk.Usage.CompletionTokens != c.comple ||
				chunk.Usage.TotalTokens != c.total {
				t.Fatalf(
					"usage = prompt %d completion %d total %d, want %d/%d/%d",
					chunk.Usage.PromptTokens,
					chunk.Usage.CompletionTokens,
					chunk.Usage.TotalTokens,
					c.prompt,
					c.comple,
					c.total,
				)
			}
		})
	}
}

// TestChatStreamDerivedTotalNotedPreFinish proves the note is recorded even
// when the derived usage arrives BEFORE the terminal (a chunk carrying both
// usage and a choice): the observability must not depend on the accounting
// arriving in phase 2.
func TestChatStreamDerivedTotalNotedPreFinish(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1,
		nil,
	)
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chunk); err != nil {
		t.Fatalf("convert pre-finish derived chunk: %v", err)
	}
	if !reportHasFeature(state.report, FeatureUsageTotalDerived) {
		t.Fatalf("pre-finish derivation recorded no note: %+v", state.report)
	}
}

// TestChatStreamDerivedTotalNeverPresentedAsSourceFact proves the derivation
// never launders an invented count into a source observation. The clamp builds
// its mismatch detail from the EMITTED counts against what the source REPORTED;
// if a derived component is still declared present, the detail claims the
// source sent a number it never sent, and an operator reading the per-request
// log cannot tell which count was invented.
//
// Two shapes, one per arithmetic family:
//   - a derived COMPONENT (completion = total - prompt) leaves input and total
//     reported, so a genuine mismatch must name the derived component absent;
//   - a derived TOTAL (total = prompt + completion) means the source reported
//     no total at all, which is never a mismatch - the two present values add
//     up to what was emitted, so no mismatch note may be raised against a
//     number the source never stated.
func TestChatStreamDerivedTotalNeverPresentedAsSourceFact(t *testing.T) {
	t.Run("derived_component_is_named_absent", func(t *testing.T) {
		// prompt 9, total 8: completion is absent, and the only defensible
		// derivation is 0 (see the impossible-subtraction case above).
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(usageTail("9", "", "8"))})
		if err != nil {
			t.Fatal(err)
		}
		state := newChatResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1,
			nil,
		)
		if _, err := state.Convert(chunk); err != nil {
			t.Fatal(err)
		}
		detail, ok := reportDetail(state.report, FeatureUsageTotalMismatch)
		if !ok {
			t.Fatalf("no mismatch note: %+v", state.report)
		}
		if strings.Contains(detail, "the source total, input, and output are relayed as-is") {
			t.Fatalf(
				"the note presents the DERIVED completion as a relayed source value: %q",
				detail,
			)
		}
		if !strings.Contains(detail, "did not report output") {
			t.Fatalf("the note must name the derived component absent: %q", detail)
		}
	})

	t.Run("derived_total_raises_no_mismatch", func(t *testing.T) {
		// prompt 5 + completion 3: the source reported no total, and the
		// derived 8 IS the exact sum, so nothing is inconsistent.
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(usageTail("5", "3", ""))})
		if err != nil {
			t.Fatal(err)
		}
		state := newChatResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1,
			nil,
		)
		if _, err := state.Convert(chunk); err != nil {
			t.Fatal(err)
		}
		if !reportHasFeature(state.report, FeatureUsageTotalDerived) {
			t.Fatalf("the derivation is not recorded: %+v", state.report)
		}
		if reportHasFeature(state.report, FeatureUsageTotalMismatch) {
			detail, _ := reportDetail(state.report, FeatureUsageTotalMismatch)
			t.Fatalf(
				"a mismatch was raised against a total the source never reported: %q",
				detail,
			)
		}
	})
}

// TestChatStreamNullTotalRejected proves an explicitly null total is an
// illegal value for a modeled scalar and rejects, never derived.
func TestChatStreamNullTotalRejected(t *testing.T) {
	for name, body := range map[string]string{
		"null_total":      usageTail("5", "3", "null"),
		"null_prompt":     usageTail("null", "3", "8"),
		"null_completion": usageTail("5", "null", "8"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)})
			if err == nil {
				t.Fatal("null total accepted")
			}
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
			}
			if !strings.Contains(err.Error(), "must not be null") {
				t.Fatalf("error = %q, want the null rejection", err.Error())
			}
		})
	}
}

// TestChatStreamOverflowSaturates proves a derived sum that would overflow is
// SATURATED, not rejected and not wrapped. The source's own usage arithmetic
// is never an exchange failure (internal/transcode/errors.go:355-362), and the
// non-streaming renderer's derivedUsageTotal already saturates and notes -
// rejecting here made the same upstream bytes succeed non-streaming and 502 on
// stream:true.
func TestChatStreamOverflowSaturates(t *testing.T) {
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`,
	)})
	if err != nil {
		t.Fatalf("the source's own arithmetic must not fail the exchange: %v", err)
	}
	if chunk.Usage == nil {
		t.Fatal("usage was dropped")
	}
	// The saturation is a bound, never a wrap: a wrapped int64 sum would land
	// negative here, which the emitted count must never be.
	if chunk.Usage.TotalTokens < 0 {
		t.Fatalf("derived total wrapped into a negative value: %d", chunk.Usage.TotalTokens)
	}
}

// TestChatStreamDerivedTotalRecordedOnce proves the derivation is observable:
// the stream state records the usage_total_derived note exactly once, and the
// decoded totals reach the emitted envelope.
func TestChatStreamDerivedTotalRecordedOnce(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1,
		nil,
	)
	// Phase 1: a content-bearing finish chunk establishes the terminal.
	finish, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(finish); err != nil {
		t.Fatalf("convert finish: %v", err)
	}
	// Phase 2: the derived usage-only tail is absorbed (and noted).
	tail, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(usageTail("5", "3", ""))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(tail); err != nil {
		t.Fatalf("convert derived tail: %v", err)
	}
	notes := 0
	for _, loss := range state.report.Losses {
		if loss.Feature == FeatureUsageTotalDerived {
			notes++
			if loss.Kind != NoteRecord {
				t.Fatalf("usage_total_derived kind = %v, want NoteRecord", loss.Kind)
			}
			if !strings.Contains(loss.Detail, "total_tokens") {
				t.Fatalf("note detail does not name the derived key: %q", loss.Detail)
			}
		}
	}
	if notes != 1 {
		t.Fatalf("usage_total_derived note count = %d, want exactly one", notes)
	}
}

// TestChatStreamDerivedTotalNotedPerKeyNotPerStream proves the
// usage_total_derived note is gated per DERIVED KEY rather than once per
// stream. A stream can derive a total in one phase and the completion in
// another; with a single per-stream gate the exchange reported the FIRST
// derivation for the whole stream, so the log named a key that was not the
// one delivered and stayed silent about the key that was. Reproduced
// before the fix: phase 1 derives total_tokens, phase 2 derives
// completion_tokens, and the report contained only the total_tokens note
// while the client received a fabricated completion_tokens.
func TestChatStreamDerivedTotalNotedPerKeyNotPerStream(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1,
		nil,
	)
	// Phase 1 derives TOTAL_tokens: prompt and completion are both present and
	// the total is absent. Phase 2 derives COMPLETION_tokens. The two phases must
	// derive DIFFERENT keys, or a once-per-stream gate also passes this test.
	finish, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(finish); err != nil {
		t.Fatalf("convert finish: %v", err)
	}
	// Phase 2: a repeated terminal carries a DIFFERENT derivable key, so
	// completion_tokens is derived from the new numbers.
	tail, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(usageTail("100", "", "150"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(tail); err != nil {
		t.Fatalf("convert second derived tail: %v", err)
	}

	noted := map[string]int{}
	for _, loss := range state.report.Losses {
		if loss.Feature == FeatureUsageTotalDerived {
			key := ""
			switch {
			case strings.Contains(loss.Detail, "completion_tokens"):
				key = "completion_tokens"
			case strings.Contains(loss.Detail, "total_tokens"):
				key = "total_tokens"
			}
			noted[key]++
		}
	}
	if noted["completion_tokens"] != 1 {
		t.Errorf("completion_tokens note count = %d, want 1; the second derivation must be recorded "+
			"with its own key, not collapsed into the first (report: %+v)",
			noted["completion_tokens"], state.report.Losses)
	}
	if noted["total_tokens"] != 1 {
		t.Errorf("total_tokens note count = %d, want 1 (report: %+v)",
			noted["total_tokens"], state.report.Losses)
	}
}

// TestChatStreamRepeatedTerminalMergeIsRecorded proves a repeated terminal
// frame that REPLACES the recorded accounting is observable. Before the fix a
// gateway redelivering a small tail over a real total silently overwrote the
// client's token counts - a {1,1,2} tail over a good {1000,500,1500} delivered
// input_tokens:1, wrong by 1000x - and the exchange was still reported as a
// clean success with no note. An exact repeat of the same values must stay
// quiet, so the note is recorded only when the values actually differ.
func TestChatStreamRepeatedTerminalMergeIsRecorded(t *testing.T) {
	finishChunk := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`

	t.Run("differing_repeat_is_recorded", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
			"resp_1", "gpt-4.1", 1, nil,
		)
		finish, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(finishChunk)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(finish); err != nil {
			t.Fatalf("convert finish: %v", err)
		}
		// A repeated terminal on the same id/model carrying DIFFERENT counts.
		repeat, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","delta":{"role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(repeat); err != nil {
			t.Fatalf("convert repeated terminal: %v", err)
		}
		var noted bool
		for _, loss := range state.report.Losses {
			if loss.Feature == FeatureUsageTotalMerged {
				noted = true
			}
		}
		if !noted {
			t.Errorf("the accounting replacement was not recorded; report: %+v", state.report.Losses)
		}
	})

	t.Run("identical_repeat_is_quiet", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
			"resp_1", "gpt-4.1", 1, nil,
		)
		finish, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(finishChunk)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(finish); err != nil {
			t.Fatalf("convert finish: %v", err)
		}
		// A role-only delta is insubstantial: the same terminal re-emitted with
		// the SAME accounting. (Repeating the content-bearing finish chunk would
		// be substantive output and is correctly rejected as wire-corrupt, which
		// is a different case and not what this asserts.)
		repeat, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","delta":{"role":"assistant"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`,
		)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(repeat); err != nil {
			t.Fatalf("convert identical repeat: %v", err)
		}
		for _, loss := range state.report.Losses {
			if loss.Feature == FeatureUsageTotalMerged {
				t.Errorf("an identical repeat must stay quiet, got: %+v", loss)
			}
		}
	})
}
