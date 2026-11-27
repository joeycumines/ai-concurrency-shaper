package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnicodeSchemaScannerDifferential(t *testing.T) {
	// None of these fragments encodes U+FFFD, so the decoder can emit it only
	// by replacing invalid Unicode. Exercise the scanner against that behavior.
	fragments := []string{
		"", "x", `\\`, `\"`, `\n`, `\t`, `\r`, `\/`,
		`\u0000`, `\uD7FF`, `\ud800`, `\uDBFF`, `\udc00`, `\uDFFF`,
		`\uE000`, `\uFFFF`, `\u005c`, `uD800`, "😀", "/", `\u007f`,
		string([]byte{0xff}), string([]byte{0xed, 0xa0, 0x80}),
		string([]byte{0xf0, 0x9f}), string([]byte{0xc0, 0xaf}),
	}
	count := 0
	for _, a := range fragments {
		for _, b := range fragments {
			for _, c := range fragments {
				input := `"` + a + b + c + `"`
				var decoded string
				if err := json.Unmarshal([]byte(input), &decoded); err != nil {
					t.Fatalf("generator produced invalid syntax %q: %v", input, err)
				}
				want := !strings.ContainsRune(decoded, '\uFFFD')
				for _, document := range []string{
					`{"const":` + input + `}`,
					`{"properties":{` + input + `:{"type":"string"}}}`,
				} {
					if got := jsonStringsDecodeLosslessly([]byte(document)); got != want {
						t.Fatalf("scanner(%q) = %v, want %v; decoded %q", document, got, want, decoded)
					}
					count++
				}
			}
		}
	}
	t.Logf("checked %d documents", count)
}
