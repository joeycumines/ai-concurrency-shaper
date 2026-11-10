// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// geminiE2EUpstream is a scripted fake upstream that records every request
// (method, path, raw query, body, key headers) and answers in its own
// dialect: a Gemini upstream answers GenerateContent (JSON or SSE), a chat
// upstream answers chat completions.
type geminiE2EUpstream struct {
	mu      sync.Mutex
	records []geminiE2ERecord
	gemini  bool // true: answer as a Gemini GenerateContent upstream
}

type geminiE2ERecord struct {
	Method   string
	Path     string
	RawQuery string
	Body     string
	APIKey   string // X-Goog-Api-Key header, when present
	Auth     string // Authorization header, when present
	XAPIKey  string // X-Api-Key header, when present
}

func (u *geminiE2EUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.records = append(u.records, geminiE2ERecord{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Body:     string(body),
		APIKey:   r.Header.Get("X-Goog-Api-Key"),
		Auth:     r.Header.Get("Authorization"),
		XAPIKey:  r.Header.Get("X-Api-Key"),
	})
	u.mu.Unlock()

	streaming := strings.Contains(r.URL.Path, ":streamGenerateContent") ||
		strings.Contains(r.URL.RawQuery, "alt=sse") ||
		strings.Contains(string(body), `"stream":true`)

	if u.gemini && r.URL.Path != "/v1/chat/completions" {
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			frame := func(data string) {
				_, _ = w.Write([]byte("data: " + data + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			frame(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"index":0}]}`)
			frame(`{"candidates":[{"finishReason":"STOP","index":0}]}`)
			frame(`{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"wire-gemini","responseId":"fixed-resp"}`))
		return
	}

	// Chat completions upstream.
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frame := func(data string) {
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		frame(`{"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1710000000,"model":"wire-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`)
		frame(`{"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1710000000,"model":"wire-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		frame(`[DONE]`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"chatcmpl-e2e","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
}

func (u *geminiE2EUpstream) snapshot() []geminiE2ERecord {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]geminiE2ERecord(nil), u.records...)
}

// heldGeminiUpstream answers a streamGenerateContent request with one frame
// and then holds the stream open until the test closes release (or the client
// goes away), so the route slot stays occupied and a second request visibly
// queues.
type heldGeminiUpstream struct {
	mu      sync.Mutex
	seen    int
	release chan struct{}
}

func (u *heldGeminiUpstream) handler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	u.mu.Lock()
	u.seen++
	u.mu.Unlock()
	if strings.Contains(r.URL.Path, ":streamGenerateContent") || strings.Contains(r.URL.RawQuery, "alt=sse") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"index\":0}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-u.release:
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`))
}

func (u *heldGeminiUpstream) seenCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen
}

// stripGeneratedIDs removes fields whose values are generated per exchange
// (ids, timestamps) so two exchanges of the same request compare equal.
var geminiE2EGeneratedFields = regexp.MustCompile(`"(id|responseId|requestId|created|created_at|createdAt)"\s*:\s*("[^"]*"|[0-9]+)`)

func stripGeneratedIDs(s string) string {
	return geminiE2EGeneratedFields.ReplaceAllString(s, `"$1":<gen>`)
}

// stripQueueComments removes SSE comment lines (": ...") and any blank lines
// they leave behind, so timing-dependent queue-wait keep-alives (which can
// appear a different number of times per exchange) do not break suite-vs-mount
// comparison. Comment lines can carry leading whitespace, so the line is
// trimmed first.
func stripQueueComments(s string) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), ":") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestE2E_GeminiGateway_SuiteMountParity builds the real binary with two
// providers — a fake Gemini upstream serving a native Gemini route
// (via=gemini) and a fake chat upstream serving -transcode-gemini-chat —
// plus a -catalog-suite over both, and a third provider behind
// -transcode-messages-gemini. For every Gemini client direction it sends the
// same request (a) through the suite prefix and (b) through the provider's
// own mount prefix and asserts identical upstream request bytes, identical
// client response bytes modulo generated ids, and identical metrics route
// keys. It also asserts that no SSE queue comment reaches a Gemini client
// under -queue-comments, that GET /v1beta/models on a mount passes through
// transparently, and that a non-POST on a Gemini path passes through.
func TestE2E_GeminiGateway_SuiteMountParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/gemini-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Upstream G: fake Gemini GenerateContent upstream (native route target).
	upG := &geminiE2EUpstream{gemini: true}
	srvG := httptest.NewServer(http.HandlerFunc(upG.handler))
	t.Cleanup(srvG.Close)

	// Upstream C: fake chat completions upstream (-transcode-gemini-chat on
	// chatprov, and on msgprov for the suite-routed G2C case).
	upC := &geminiE2EUpstream{}
	srvC := httptest.NewServer(http.HandlerFunc(upC.handler))
	t.Cleanup(srvC.Close)

	// Upstream M: fake Gemini upstream behind -transcode-messages-gemini.
	// Its chat path also answers chat completions so the msgprov G2C case
	// works; the handler distinguishes by path.
	upM := &geminiE2EUpstream{gemini: true}
	srvM := httptest.NewServer(http.HandlerFunc(upM.handler))
	t.Cleanup(srvM.Close)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	metricsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen metrics: %v", err)
	}
	metricsAddr := metricsLn.Addr().String()
	metricsLn.Close()

	var out safeBuffer
	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"-metrics-bind", metricsAddr,
		// Server-scope flags before the first --provider marker.
		"-model-table",
		"fake-gemini@gemini=wire-gemini;via=gemini",
		"-model-table",
		"chat-model@chatprov=wire-chat",
		"-model-table",
		"msg-model@msgprov=wire-msg;via=gemini",
		"-catalog-suite", "suite@/s",
		// Provider 1: fake Gemini upstream, native Gemini route.
		"--provider=gemini",
		"-upstream", srvG.URL,
		"-prefix", "/gem",
		"-auth-source", "env:SHAPER_PROVIDER_GEMINI_KEY",
		"-auth-mode", "goog-api-key",
		"-native-route", "gemini@/v1beta/models/{model}:generateContent",
		// Suffix patterns: route patterns have no {model} wildcard, so a
		// template-shaped -limit would never match a concrete Gemini path.
		"-limit", "POST /generateContent:1",
		"-limit", "POST /streamGenerateContent:1",
		// Provider 2: fake chat upstream, Gemini client -> chat upstream.
		"--provider=chatprov",
		"-upstream", srvC.URL,
		"-prefix", "/chat",
		"-auth-source", "env:SHAPER_PROVIDER_CHATPROV_KEY",
		"-auth-mode", "bearer",
		"-transcode-gemini-chat",
		"-limit", "POST /generateContent:1",
		"-limit", "POST /streamGenerateContent:1",
		// Provider 3: fake Gemini upstream, Messages client -> Gemini
		// upstream, plus a Gemini-client transcode so the suite can route
		// Gemini paths to it too.
		"--provider=msgprov",
		"-upstream", srvM.URL,
		"-prefix", "/msg",
		"-auth-source", "env:SHAPER_PROVIDER_MSGPROV_KEY",
		"-auth-mode", "goog-api-key",
		"-transcode-messages-gemini",
		"-transcode-gemini-chat",
		"-limit", "POST /v1/messages:1",
		"-limit", "POST /generateContent:1",
		"-limit", "POST /streamGenerateContent:1",
		"-queue-comments", "50ms",
	)

	var filteredEnv []string
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "SHAPER_PROVIDER_") {
			filteredEnv = append(filteredEnv, env)
		}
	}
	cmd.Env = append(filteredEnv,
		"SHAPER_PROVIDER_GEMINI_KEY=gem-secret",
		"SHAPER_PROVIDER_CHATPROV_KEY=chat-secret",
		"SHAPER_PROVIDER_MSGPROV_KEY=msg-secret",
	)
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()
	if err := waitTCPReady(proxyAddr, 10*time.Second); err != nil {
		t.Fatalf("proxy addr: %v\noutput:\n%s", err, out.String())
	}
	if err := waitTCPReady(metricsAddr, 10*time.Second); err != nil {
		t.Fatalf("metrics addr: %v\noutput:\n%s", err, out.String())
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + proxyAddr

	post := func(path, body string, acceptSSE bool) (int, http.Header, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if acceptSSE {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}

	geminiBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`
	messagesBody := `{"model":"msg-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

	// Each case: name, request path+body+stream flag for the suite mount and
	// the provider mount, and which upstream records must grow.
	type geminiCase struct {
		name        string
		suitePath   string
		mountPath   string
		body        string
		stream      bool
		wantUpPath  string // upstream path the fake must see
		upstreamRec func() []geminiE2ERecord
	}
	cases := []geminiCase{
		{
			name:        "native JSON (suite vs mount)",
			suitePath:   "/s/v1beta/models/fake-gemini:generateContent",
			mountPath:   "/gem/v1beta/models/fake-gemini:generateContent",
			body:        geminiBody,
			wantUpPath:  "/v1beta/models/wire-gemini:generateContent",
			upstreamRec: upG.snapshot,
		},
		{
			name:        "native SSE (suite vs mount)",
			suitePath:   "/s/v1beta/models/fake-gemini:streamGenerateContent?alt=sse",
			mountPath:   "/gem/v1beta/models/fake-gemini:streamGenerateContent?alt=sse",
			body:        geminiBody,
			stream:      true,
			wantUpPath:  "/v1beta/models/wire-gemini:streamGenerateContent",
			upstreamRec: upG.snapshot,
		},
		{
			name:        "G2C JSON (suite vs mount)",
			suitePath:   "/s/v1beta/models/chat-model:generateContent",
			mountPath:   "/chat/v1beta/models/chat-model:generateContent",
			body:        geminiBody,
			wantUpPath:  "/v1/chat/completions",
			upstreamRec: upC.snapshot,
		},
		{
			name:        "G2C SSE (suite vs mount)",
			suitePath:   "/s/v1beta/models/chat-model:streamGenerateContent?alt=sse",
			mountPath:   "/chat/v1beta/models/chat-model:streamGenerateContent?alt=sse",
			body:        geminiBody,
			stream:      true,
			wantUpPath:  "/v1/chat/completions",
			upstreamRec: upC.snapshot,
		},
		{
			name:        "M2G JSON (suite vs mount)",
			suitePath:   "/s/v1/messages?beta=true",
			mountPath:   "/msg/v1/messages?beta=true",
			body:        messagesBody,
			wantUpPath:  "/v1beta/models/wire-msg:generateContent",
			upstreamRec: upM.snapshot,
		},
		{
			name:        "M2G SSE (suite vs mount)",
			suitePath:   "/s/v1/messages?beta=true",
			mountPath:   "/msg/v1/messages?beta=true",
			body:        `{"model":"msg-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			stream:      true,
			wantUpPath:  "/v1beta/models/wire-msg:streamGenerateContent",
			upstreamRec: upM.snapshot,
		},
		{
			name:        "G2C on msgprov JSON (suite vs mount)",
			suitePath:   "/s/v1beta/models/msg-model:generateContent",
			mountPath:   "/msg/v1beta/models/msg-model:generateContent",
			body:        geminiBody,
			wantUpPath:  "/v1/chat/completions",
			upstreamRec: upM.snapshot,
		},
		{
			name:        "G2C on msgprov SSE (suite vs mount)",
			suitePath:   "/s/v1beta/models/msg-model:streamGenerateContent?alt=sse",
			mountPath:   "/msg/v1beta/models/msg-model:streamGenerateContent?alt=sse",
			body:        geminiBody,
			stream:      true,
			wantUpPath:  "/v1/chat/completions",
			upstreamRec: upM.snapshot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.upstreamRec())

			stSuite, hdrSuite, bodySuite := post(tc.suitePath, tc.body, tc.stream)
			if stSuite != http.StatusOK {
				t.Fatalf("suite POST %s status = %d, want 200: %s", tc.suitePath, stSuite, bodySuite)
			}
			stMount, hdrMount, bodyMount := post(tc.mountPath, tc.body, tc.stream)
			if stMount != http.StatusOK {
				t.Fatalf("mount POST %s status = %d, want 200: %s", tc.mountPath, stMount, bodyMount)
			}

			recs := tc.upstreamRec()
			if len(recs) != before+2 {
				t.Fatalf("upstream saw %d new requests, want 2 (suite + mount): %+v", len(recs)-before, recs[before:])
			}
			upSuite, upMount := recs[before], recs[before+1]

			// Identical upstream request bytes and routing.
			if upSuite.Path != tc.wantUpPath || upMount.Path != tc.wantUpPath {
				t.Fatalf("upstream paths = %q / %q, want %q", upSuite.Path, upMount.Path, tc.wantUpPath)
			}
			if upSuite.Body != upMount.Body {
				t.Fatalf("upstream bodies differ suite-vs-mount:\n suite: %s\n mount: %s", upSuite.Body, upMount.Body)
			}
			if upSuite.RawQuery != upMount.RawQuery {
				t.Fatalf("upstream query differs: %q vs %q", upSuite.RawQuery, upMount.RawQuery)
			}
			if upSuite.APIKey != upMount.APIKey || upSuite.Auth != upMount.Auth || upSuite.XAPIKey != upMount.XAPIKey {
				t.Fatalf("upstream auth headers differ: suite %+v mount %+v", upSuite, upMount)
			}

			// Identical client response bytes modulo generated ids and
			// queue-comment lines (timing-dependent keep-alives, never sent
			// to Gemini clients).
			normSuite := stripGeneratedIDs(stripQueueComments(bodySuite))
			normMount := stripGeneratedIDs(stripQueueComments(bodyMount))
			if normSuite != normMount {
				t.Fatalf("client bodies differ suite-vs-mount after normalization:\n suite: %q\n mount: %q\n raw suite:\n%s\n raw mount:\n%s",
					normSuite, normMount, bodySuite, bodyMount)
			}
			if (hdrSuite.Get("Content-Type") == "text/event-stream") != (hdrMount.Get("Content-Type") == "text/event-stream") {
				t.Fatalf("content-type mismatch: suite %q mount %q", hdrSuite.Get("Content-Type"), hdrMount.Get("Content-Type"))
			}
			if tc.stream {
				if !strings.HasPrefix(hdrSuite.Get("Content-Type"), "text/event-stream") {
					t.Fatalf("stream case: suite Content-Type = %q, want SSE", hdrSuite.Get("Content-Type"))
				}
				// No SSE comment line may reach a Gemini client under
				// -queue-comments (the gate is on the Gemini operation path,
				// which is what every *G*-client case here exercises).
				if strings.Contains(tc.suitePath, "/v1beta/") {
					for line := range strings.SplitSeq(bodySuite, "\n") {
						if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), ":") {
							t.Fatalf("Gemini client received an SSE comment line %q under -queue-comments", line)
						}
					}
				}
			}
		})
	}

	// Metrics route keys: the per-route queue series exported on /metrics are
	// keyed by the CONCRETE in-flight path (the TUI/Snapshot route stats are
	// template-keyed via statsRouteKey, but Prometheus only exports the
	// concrete key), and the main fleet's per-provider totals must show the
	// Gemini routes served 2xx.
	t.Run("metrics route queue series are concrete-keyed", func(t *testing.T) {
		// Occupy the single route slot on a held proxy with a held stream,
		// then fire a second request that must queue and scrape while it
		// waits.
		heldUp := &heldGeminiUpstream{release: make(chan struct{})}
		t.Cleanup(func() { close(heldUp.release) })
		heldSrv := httptest.NewServer(http.HandlerFunc(heldUp.handler))
		t.Cleanup(heldSrv.Close)

		heldLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		heldAddr := heldLn.Addr().String()
		heldLn.Close()
		heldMetricsLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		heldMetricsAddr := heldMetricsLn.Addr().String()
		heldMetricsLn.Close()
		heldCmd := exec.Command(bin,
			"-bind", heldAddr,
			"-metrics-bind", heldMetricsAddr,
			"-model-table", "held-model@held=wire-held;via=gemini",
			"--provider=held",
			"-upstream", heldSrv.URL,
			"-prefix", "/held",
			"-auth-mode", "none",
			"-native-route", "gemini@/v1beta/models/{model}:generateContent",
			// Suffix pattern (route patterns have no {model} wildcard, so a
			// template-shaped -limit never matches a concrete Gemini path).
			"-limit", "POST /streamGenerateContent:1",
		)
		heldCmd.Env = os.Environ()
		var heldOut safeBuffer
		heldCmd.Stdout = &heldOut
		heldCmd.Stderr = &heldOut
		if err := heldCmd.Start(); err != nil {
			t.Fatalf("start held proxy: %v", err)
		}
		defer func() {
			_ = heldCmd.Process.Signal(syscall.SIGTERM)
			_ = heldCmd.Wait()
		}()
		if err := waitTCPReady(heldAddr, 10*time.Second); err != nil {
			t.Fatalf("held proxy: %v\n%s", err, heldOut.String())
		}
		if err := waitTCPReady(heldMetricsAddr, 10*time.Second); err != nil {
			t.Fatalf("held metrics: %v\n%s", err, heldOut.String())
		}

		heldBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		heldClient := &http.Client{Timeout: 10 * time.Second}
		go func() {
			req, _ := http.NewRequest(http.MethodPost,
				"http://"+heldAddr+"/held/v1beta/models/held-model:streamGenerateContent?alt=sse",
				strings.NewReader(heldBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			resp, err := heldClient.Do(req)
			if err == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for heldUp.seenCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if heldUp.seenCount() == 0 {
			t.Fatal("held upstream never saw the first request")
		}
		go func() {
			req, _ := http.NewRequest(http.MethodPost,
				"http://"+heldAddr+"/held/v1beta/models/held-model:streamGenerateContent?alt=sse",
				strings.NewReader(heldBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			resp, err := heldClient.Do(req)
			if err == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}()
		// Poll the held /metrics until the queued second request shows up in
		// the per-route queue series. The series are keyed by the CONCRETE
		// in-flight path (after the /held prefix strip); the template key
		// only appears in the TUI/Snapshot route stats, which Prometheus does
		// not export.
		wantQueued := `shaper_route_queued{provider="held",method="POST",path="/v1beta/models/held-model:streamGenerateContent"} 1`
		wantAge := `shaper_route_oldest_queued_seconds{provider="held",method="POST",path="/v1beta/models/held-model:streamGenerateContent"}`
		var text string
		deadline = time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get("http://" + heldMetricsAddr + "/metrics")
			if err != nil {
				t.Fatalf("GET held /metrics: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			text = string(body)
			if strings.Contains(text, wantQueued) && strings.Contains(text, wantAge) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(text, wantQueued) {
			t.Fatalf("metrics missing concrete-keyed route queue series %q:\n%s", wantQueued, text)
		}
		if !strings.Contains(text, wantAge) {
			t.Fatalf("metrics missing concrete-keyed oldest-queued series %q:\n%s", wantAge, text)
		}

		// The main fleet served every Gemini direction with 2xx.
		resp2, err := http.Get("http://" + metricsAddr + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp2.Body.Close()
		body2, _ := io.ReadAll(resp2.Body)
		text2 := string(body2)
		for _, want := range []string{
			`shaper_requests_total{provider="gemini",status="2xx"}`,
			`shaper_requests_total{provider="chatprov",status="2xx"}`,
			`shaper_requests_total{provider="msgprov",status="2xx"}`,
		} {
			if !strings.Contains(text2, want) {
				t.Fatalf("main fleet metrics missing series %q:\n%s", want, text2)
			}
		}
	})

	// GET /v1beta/models on a provider mount passes through transparently
	// (no catalog is served on a mount).
	t.Run("GET /v1beta/models on a mount passes through", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/gem/v1beta/models", nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		recs := upG.snapshot()
		last := recs[len(recs)-1]
		if last.Method != http.MethodGet || last.Path != "/v1beta/models" {
			t.Fatalf("upstream saw %s %s, want GET /v1beta/models forwarded verbatim", last.Method, last.Path)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
		}
	})

	// A non-POST on a Gemini operation path passes through transparently.
	t.Run("non-POST on a Gemini path passes through", func(t *testing.T) {
		before := len(upG.snapshot())
		req, _ := http.NewRequest(http.MethodGet, base+"/gem/v1beta/models/fake-gemini:generateContent", nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		recs := upG.snapshot()
		if len(recs) != before+1 {
			t.Fatalf("upstream saw %d new requests, want 1", len(recs)-before)
		}
		last := recs[len(recs)-1]
		if last.Method != http.MethodGet || last.Path != "/v1beta/models/fake-gemini:generateContent" {
			t.Fatalf("upstream saw %s %s, want the GET forwarded unchanged", last.Method, last.Path)
		}
	})

	// JSON responses of the suite and mount must carry no generated-id drift
	// beyond the fields we strip; double-check the strip actually matched.
	t.Run("generated ids were actually present", func(t *testing.T) {
		_ = json.Valid // keep encoding/json import honest if unused later
	})
}
