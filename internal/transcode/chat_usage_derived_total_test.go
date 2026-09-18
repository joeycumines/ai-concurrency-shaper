package transcode

// Tests for deriving a single missing usage total on a chat stream tail.
//
// The pinned CompletionUsage requires prompt/completion/total together, but an
// honest upstream tail can omit exactly one. A single missing total is derived
// from the two present values (never defaulted to zero) and the derivation is
// recorded as the usage_total_derived note; two or more missing totals cannot
// be derived and remain a typed upstream wire error, as does an impossible
// subtraction (total smaller than a component).

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

// TestChatStreamImpossibleDerivationRejected proves a subtraction that would
// produce a negative component is rejected rather than emitted.
func TestChatStreamImpossibleDerivationRejected(t *testing.T) {
	for name, body := range map[string]string{
		"total_lt_prompt":     usageTail("9", "", "8"),
		"total_lt_completion": usageTail("", "9", "8"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)})
			if err == nil {
				t.Fatal("impossible derivation accepted")
			}
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
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

// TestChatStreamOverflowRejected proves a derived sum that would overflow is
// rejected rather than wrapping into a wrong value.
func TestChatStreamOverflowRejected(t *testing.T) {
	_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`,
	)})
	if err == nil {
		t.Fatal("overflow accepted")
	}
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
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
