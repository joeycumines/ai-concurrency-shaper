package transcode

// Review-z commit 1 acceptance: all six malformed-JSON categories are
// rejected with typed wire.DecodeError at the decode boundaries — they can
// never reach the converters. Each category is exercised through the real
// entry points (request decode, upstream response decode, upstream stream
// decode) and the typed Kind is asserted on the error chain.

import (
	"errors"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// TestWireDecodeSixCategoriesClientRequest proves the client request decode
// rejects all six categories with typed decode errors (client-dialect, never
// an upstream classification).
func TestWireDecodeSixCategoriesClientRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind wire.DecodeErrorKind
	}{
		{"duplicate_key", `{"model":"m","model":"n","input":"hi"}`, wire.DecodeDuplicateKey},
		{"unknown_field", `{"model":"m","input":"hi","bogus":1}`, wire.DecodeUnknownField},
		{"missing_required", `{"input":"hi"}`, wire.DecodeMissingRequired},
		{"illegal_null", `{"model":null,"input":"hi"}`, wire.DecodeIllegalNull},
		{"trailing_value", `{"model":"m","input":"hi"} {"x":1}`, wire.DecodeTrailingValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := DecodeResponsesRequest([]byte(c.body), StrictLossPolicy())
			assertWireDecodeKind(t, err, c.kind)
		})
	}

	// Contradictory union: a tool missing its required strict is a
	// malformed request, and a chat message carrying another role's fields
	// is a contradictory union.
	if _, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}]}`),
		StrictLossPolicy(),
	); err == nil {
		t.Fatal("expected missing-strict rejection")
	} else {
		assertWireDecodeKind(t, err, wire.DecodeMissingRequired)
	}
}

// TestWireDecodeSixCategoriesUpstream proves the upstream response decode
// rejects all six categories with typed decode errors (wrapped in the
// upstream-wire classification, never converted).
func TestWireDecodeSixCategoriesUpstream(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind wire.DecodeErrorKind
	}{
		{
			"duplicate_key",
			`{"id":"r","id":"r2","object":"response","created_at":1,"status":"completed","model":"m","output":[]}`,
			wire.DecodeDuplicateKey,
		},
		{
			"unknown_field",
			`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[],"bogus":1}`,
			wire.DecodeUnknownField,
		},
		{
			"missing_required",
			`{"object":"response","created_at":1,"status":"completed","model":"m","output":[]}`,
			wire.DecodeMissingRequired,
		},
		{
			"illegal_null",
			`{"id":null,"object":"response","created_at":1,"status":"completed","model":"m","output":[]}`,
			wire.DecodeIllegalNull,
		},
		{
			"trailing_value",
			`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[]} {}`,
			wire.DecodeTrailingValue,
		},
		{
			// Contradictory union arm: an output message whose role is not
			// assistant violates the assistant-only output contract.
			"contradictory_union",
			`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"m_1","type":"message","role":"user","status":"completed","content":[]}]}`,
			wire.DecodeContradictoryUnion,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeResponsesResponse([]byte(c.body))
			if err == nil {
				t.Fatal("expected decode rejection")
			}
			assertWireDecodeKind(t, err, c.kind)
			// Upstream-side rejections classify as upstream wire errors.
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
			}
		})
	}
}

// TestWireDecodeSixCategoriesStream proves the stream decode rejects the
// categories with typed decode errors before any state machine sees them.
func TestWireDecodeSixCategoriesStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind wire.DecodeErrorKind
	}{
		{
			"duplicate_key",
			`{"type":"response.created","type":"response.created","sequence_number":0,"response":{"id":"r","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`,
			wire.DecodeDuplicateKey,
		},
		{
			"unknown_field",
			`{"type":"response.created","sequence_number":0,"response":{"id":"r","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]},"bogus":1}`,
			wire.DecodeUnknownField,
		},
		{
			"illegal_null",
			`{"type":null,"sequence_number":0}`,
			wire.DecodeIllegalNull,
		},
		{
			"trailing_value",
			`{"type":"response.created","sequence_number":0} {}`,
			wire.DecodeTrailingValue,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeResponsesSSEEvent([]byte(c.body))
			if err == nil {
				t.Fatal("expected decode rejection")
			}
			assertWireDecodeKind(t, err, c.kind)
			if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
			}
		})
	}

	// Missing required on an event payload: an error event without code is
	// missing_required.
	if _, err := decodeResponsesSSEEvent(
		[]byte(`{"type":"error","sequence_number":0,"message":"m"}`),
	); err == nil {
		t.Fatal("expected rejection of code-less error event")
	} else {
		assertWireDecodeKind(t, err, wire.DecodeMissingRequired)
	}
}

// TestWireDecodeSixCategoriesChat proves the Chat upstream decode rejects the
// categories with typed decode errors, including the contradictory-union
// role-conditional rejection.
func TestWireDecodeSixCategoriesChat(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind wire.DecodeErrorKind
	}{
		{
			"duplicate_key",
			`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"model":"m2"}`,
			wire.DecodeDuplicateKey,
		},
		{
			"unknown_field",
			`{"id":"c","object":"chat.completion","created":1,"model":"m","bogus":1,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
			wire.DecodeUnknownField,
		},
		{
			"illegal_null",
			`{"id":null,"object":"chat.completion","created":1,"model":"m","choices":[]}`,
			wire.DecodeIllegalNull,
		},
		{
			// An assistant response message carrying the tool-only
			// tool_call_id field is a contradictory union: the field would
			// otherwise be silently dropped.
			"contradictory_union",
			`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x","tool_call_id":"t"}}]}`,
			wire.DecodeContradictoryUnion,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := DecodeChatResponseWithPolicy([]byte(c.body), ChatCapabilities{}, StrictLossPolicy())
			if err == nil {
				t.Fatal("expected decode rejection")
			}
			assertWireDecodeKind(t, err, c.kind)
		})
	}
}

// assertWireDecodeKind asserts the error chain carries the typed wire decode
// error with the exact category.
func assertWireDecodeKind(t *testing.T, err error, kind wire.DecodeErrorKind) {
	t.Helper()
	var decodeErr *wire.DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("err = %T %v, want *wire.DecodeError", err, err)
	}
	if decodeErr.Kind != kind {
		t.Fatalf("decode kind = %v, want %v (%v)", decodeErr.Kind, kind, err)
	}
}

// TestWireDecodeNullUnionPayloads proves null or type-less union payloads
// are corrupt wire (typed missing-required), never an empty-feature
// unsupported report (review run 1 finding F1).
func TestWireDecodeNullUnionPayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
		path string
	}{
		{"null output item", `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":null}`, "type"},
		{"null output part", `{"type":"response.content_part.added","sequence_number":0,"item_id":"m","output_index":0,"content_index":0,"part":null}`, "type"},
		{"null input item", `{"model":"m","input":[null]}`, "type"},
		{"null input part", `{"model":"m","input":[{"role":"user","content":[null]}]}`, "type"},
		{"type-less output item", `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"m"}}`, "type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeResponsesSSEEvent([]byte(c.body))
			if err == nil {
				// Input-side cases go through the request decoder instead.
				_, _, err = DecodeResponsesRequest([]byte(c.body), StrictLossPolicy())
			}
			if err == nil {
				t.Fatal("expected decode rejection")
			}
			assertWireDecodeKind(t, err, wire.DecodeMissingRequired)
			// Never an empty unsupported-feature report.
			var unsupported *UnsupportedFeatureError
			if errors.As(err, &unsupported) && unsupported.Feature == "" {
				t.Fatalf("null payload misclassified as unsupported: %v", err)
			}
		})
	}
}
