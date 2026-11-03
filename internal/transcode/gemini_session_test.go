package transcode

import "testing"

func TestFirstUserTextGeminiContents(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"model","parts":[{"text":"earlier answer"}]},
		{"role":"user","parts":[{"text":"hi"},{"text":"reasoning","thought":true},{"text":" there"}]},
		{"role":"user","parts":[{"text":"later"}]}
	]}`)
	if got := FirstUserText(NativeGemini, body); got != "hi there" {
		t.Fatalf("FirstUserText = %q, want the first user content's non-thought text", got)
	}
	roleless := []byte(`{"contents":[{"parts":[{"text":"no role"}]}]}`)
	if got := FirstUserText(NativeGemini, roleless); got != "no role" {
		t.Fatalf("roleless FirstUserText = %q", got)
	}
}

func TestClientProtocolToNativeGemini(t *testing.T) {
	if got := clientProtocolToNative(ClientGemini); got != NativeGemini {
		t.Fatalf("clientProtocolToNative(gemini) = %q, want %q", got, NativeGemini)
	}
	if got := clientProtocolToNative(ClientMessages); got != NativeMessages {
		t.Fatalf("clientProtocolToNative(messages) = %q", got)
	}
}
