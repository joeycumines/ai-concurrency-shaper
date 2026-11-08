package transcode

import (
	"errors"
	"testing"
)

// TestDecodeGeminiResponseRefusedResponseReportsNothing proves a refused
// non-streaming decode returns no report entries: the id-less call's note
// describes a call the refused response never delivers.
func TestDecodeGeminiResponseRefusedResponseReportsNothing(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}},{"text":"hi","partMetadata":{"foo":1}}]},"finishReason":"STOP","index":0}],"modelVersion":"m"}`
	_, report, err := DecodeGeminiResponse(
		[]byte(body), ChatCapabilities{}, LossPolicy{Allowed: map[Feature]struct{}{}}, NewExchangeIDs(),
	)
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureGeminiPartMetadata) {
		t.Fatalf("err = %T %v, want the refused part metadata", err, err)
	}
	if len(report.Losses) != 0 || report.Dropped != 0 {
		t.Fatalf("refused decode returned report %+v, want no entries", report)
	}
}

// TestDecodeGeminiResponseRejectsNamelessFunctionCall proves a function call
// without a name is an upstream wire fault on both paths: the stream already
// refuses it, and the non-streaming decode must agree.
func TestDecodeGeminiResponseRejectsNamelessFunctionCall(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"args":{}}}]},"finishReason":"STOP","index":0}],"modelVersion":"m"}`
	_, _, err := DecodeGeminiResponse(
		[]byte(body), ChatCapabilities{}, StrictLossPolicy(), NewExchangeIDs(),
	)
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("non-streaming err = %T %v, want an upstream wire fault", err, err)
	}
	_, err = newGeminiStreamState(t, nil).Convert(geminiChunk(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"args":{}}}]},"index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("stream err = %T %v, want an upstream wire fault", err, err)
	}
}
