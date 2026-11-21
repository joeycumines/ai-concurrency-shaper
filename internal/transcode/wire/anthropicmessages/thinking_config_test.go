package anthropicmessages

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestThinkingConfigRoundTripsLegalArms(t *testing.T) {
	for _, body := range []string{
		`{"type":"enabled","budget_tokens":4096}`,
		`{"type":"enabled","budget_tokens":4096,"display":"summarized"}`,
		`{"type":"enabled","budget_tokens":4096,"display":"omitted"}`,
		`{"type":"enabled","budget_tokens":4096,"display":"updates"}`,
		`{"type":"adaptive"}`,
		`{"type":"adaptive","display":"summarized"}`,
		`{"type":"adaptive","display":"omitted"}`,
		`{"type":"adaptive","display":"updates"}`,
		`{"type":"disabled"}`,
		`{"type":"between_tools"}`,
	} {
		var config ThinkingConfig
		if err := json.Unmarshal([]byte(body), &config); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != body {
			t.Fatalf("thinking round trip = %s, want %s", encoded, body)
		}
	}
}

func TestThinkingConfigNullDisplayIsOnlyNullableOnItsOwnArms(t *testing.T) {
	for _, body := range []string{
		`{"type":"enabled","budget_tokens":4096,"display":null}`,
		`{"type":"adaptive","display":null}`,
	} {
		var config ThinkingConfig
		if err := json.Unmarshal([]byte(body), &config); err != nil {
			t.Fatal(err)
		}
		if config.Display != nil {
			t.Fatalf("null display became a value: %+v", config)
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"display"`) {
			t.Fatalf("absent display was invented: %s", encoded)
		}
	}
	for _, kind := range []string{"disabled", "between_tools"} {
		for _, field := range []string{`"display":null`, `"display":"updates"`, `"budget_tokens":null`, `"budget_tokens":4096`} {
			body := `{"type":"` + kind + `",` + field + `}`
			var config ThinkingConfig
			err := json.Unmarshal([]byte(body), &config)
			var typed *wire.DecodeError
			if !errors.As(err, &typed) || typed.Kind != wire.DecodeUnknownField {
				t.Fatalf("%s: error = %v, want per-arm unknown-field rejection", body, err)
			}
		}
	}
	var config ThinkingConfig
	if err := json.Unmarshal([]byte(`{"type":"adaptive","budget_tokens":null}`), &config); err == nil {
		t.Fatal("adaptive accepted budget_tokens because it was null")
	}
}

func TestThinkingConfigRejectsMalformedValues(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"type":null}`, `{"type":"unknown"}`,
		`{"type":"enabled"}`,
		`{"type":"enabled","budget_tokens":null}`,
		`{"type":"enabled","budget_tokens":0}`,
		`{"type":"enabled","budget_tokens":-1}`,
		`{"type":"enabled","budget_tokens":"4096"}`,
		`{"type":"adaptive","display":""}`,
		`{"type":"adaptive","display":"bogus"}`,
		`{"type":"adaptive","display":1}`,
		`{"type":"adaptive","display":"updates","display":"omitted"}`,
		`{"type":"enabled","budget_tokens":4096,"unknown":null}`,
	} {
		var config ThinkingConfig
		if err := json.Unmarshal([]byte(body), &config); err == nil {
			t.Fatalf("malformed thinking accepted: %s", body)
		}
	}
	budget, display := 4096, "updates"
	for _, config := range []ThinkingConfig{
		{Type: "enabled"},
		{Type: "disabled", Display: &display},
		{Type: "between_tools", BudgetTokens: &budget},
		{Type: "adaptive", BudgetTokens: &budget},
	} {
		if _, err := json.Marshal(config); err == nil {
			t.Fatalf("invalid hand-built thinking rendered: %+v", config)
		}
	}
}
