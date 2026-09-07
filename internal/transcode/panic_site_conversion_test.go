package transcode

// Autopsy 2026-09-06 M5: the conversion ran on the stream-copy goroutine
// with no recover, so a panic at either explicit panic site killed the
// process. Both sites are unreachable-by-construction internal invariants;
// they now surface typed errors so a future invariant break degrades the
// exchange (clean error terminal, sealed downstream, closed upstream body,
// released slot) instead of the process.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTypedResponsesEventUnknownTypeIsErrorNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("typedResponsesEvent panicked on an unmapped event type: %v", r)
		}
	}()
	_, err := typedResponsesEvent(&unknownResponsesEventStub{})
	if err == nil {
		t.Fatal("an unmapped event type must produce an error")
	}
	if !strings.Contains(err.Error(), "not mapped to a typed event") {
		t.Fatalf("err = %v, want the unmapped-event-type error", err)
	}
}

type unknownResponsesEventStub struct{}

func (unknownResponsesEventStub) EventType() string { return "response.unknown_stub" }
func (unknownResponsesEventStub) Validate() error   { return nil }
func (unknownResponsesEventStub) Sequence() int64   { return 0 }

func TestRawMessageInvalidValueIsErrorNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("rawMessage panicked on an invalid value: %v", r)
		}
	}()
	_, err := rawMessage(map[string]json.RawMessage{"k": json.RawMessage("not json")})
	if err == nil {
		t.Fatal("an invalid raw value must produce an error")
	}
	if !strings.Contains(err.Error(), "tool arguments object") {
		t.Fatalf("err = %v, want the tool-arguments-object error", err)
	}
	// Valid values still marshal.
	raw, err := rawMessage(map[string]json.RawMessage{"k": json.RawMessage(`"v"`)})
	if err != nil {
		t.Fatalf("valid map must marshal: %v", err)
	}
	if string(raw) != `{"k":"v"}` {
		t.Fatalf("raw = %s", raw)
	}
}

func TestParseToolArgumentsInvalidJSONFallsBackToRaw(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseToolArguments panicked: %v", r)
		}
	}()
	out := ParseToolArguments("not an object")
	if out.IsObject {
		t.Fatal("non-object input must not be flagged as object")
	}
	if out.Raw != "not an object" {
		t.Fatalf("raw = %q", out.Raw)
	}
}
