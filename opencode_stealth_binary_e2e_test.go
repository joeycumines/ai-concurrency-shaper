package main

// Out-of-process acceptance for the opencode stealth shape: the real binary
// is started with an opencode-provider configuration (native routes, the via
// model table, the first-party header preset, a catalog suite) and driven
// over real sockets with claude-code-shaped traffic.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type opencodeUpstream struct {
	mu      sync.Mutex
	path    string
	model   string
	session string
	ua      string
	beta    string
	bodies  [][]byte
}

func (u *opencodeUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	u.mu.Lock()
	u.path = r.URL.Path
	u.model = probe.Model
	u.session = r.Header.Get("X-Opencode-Session")
	u.ua = r.Header.Get("User-Agent")
	u.beta = r.URL.Query().Get("beta")
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/messages":
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"wire-msg","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	case "/v1/chat/completions":
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"wire-chat","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	case "/v1/responses":
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"wire-resp","status":"completed","output":[]}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func (u *opencodeUpstream) got() (path, model, session, ua, beta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.model, u.session, u.ua, u.beta
}

// TestE2E_OpencodeStealth_RealBinary drives the exact configuration shape the
// operator runs for an opencode provider: native messages and chat routes, the
// via model table, -opencode, and a catalog suite in front.
func TestE2E_OpencodeStealth_RealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/opencode-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	up := &opencodeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyAddr := ln.Addr().String()
	ln.Close()

	var out safeBuffer
	cmd := exec.Command(bin,
		// Server scope: the global table and the suite mount.
		"-bind", proxyAddr,
		"-model-table", "claude-alias@opencode-go=wire-msg;context=200000;via=messages",
		"-model-table", "chat-alias@opencode-go=wire-chat;context=1000000;via=chat",
		"-model-table", "gpt-alias@opencode-go=wire-resp;context=400000;via=responses",
		"-model-table", "zen-claude@zen=wire-msg;context=200000;via=messages",
		"-catalog-suite=/suite=claude-alias+chat-alias+gpt-alias;format=anthropic",
		// An opencode Go mount: native routes plus the first-party preset.
		"--provider=opencode-go",
		"-upstream", srv.URL,
		"-prefix", "/opencode-go",
		"-auth-source", "env:SHAPER_PROVIDER_OPENCODE_API_KEY",
		"-auth-mode", "bearer",
		"-opencode",
		"-native-route", "messages@/v1/messages",
		"-native-route", "chat@/v1/chat/completions",
		"-native-route", "responses@/v1/responses",
		"-transcode-messages-chat",
		"--provider=zen",
		"-upstream", srv.URL,
		"-prefix", "/zen",
		"-native-route", "messages@/v1/messages",
	)
	filteredEnv := []string{}
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "SHAPER_PROVIDER_") {
			filteredEnv = append(filteredEnv, env)
		}
	}
	cmd.Env = append(filteredEnv, "SHAPER_PROVIDER_OPENCODE_API_KEY=test-opencode-key")
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
	if err := waitTCPReady(proxyAddr, 5*time.Second); err != nil {
		t.Fatalf("proxy addr: %v\noutput:\n%s", err, out.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}
	post := func(target, body string, headers map[string]string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, "http://"+proxyAddr+target, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v\noutput:\n%s", target, err, out.String())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	claudeHeaders := map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
	}

	// Headerless claude-code shape through the suite: the reported failure.
	code, body := post("/suite/v1/messages?beta=true",
		`{"model":"claude-alias","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, claudeHeaders)
	if code != 200 {
		t.Fatalf("messages status = %d: %s", code, body)
	}
	path, model, session, ua, beta := up.got()
	if path != "/v1/messages" || model != "wire-msg" {
		t.Fatalf("upstream path=%q model=%q, want /v1/messages wire-msg", path, model)
	}
	if session == "" {
		t.Fatal("upstream session missing: a headerless client must still carry one")
	}
	if !strings.HasPrefix(ua, "opencode/") {
		t.Fatalf("upstream User-Agent = %q, want the first-party shape (a client's own User-Agent must not reach the upstream)", ua)
	}
	if beta != "true" {
		t.Fatalf("upstream beta = %q, want the client query preserved", beta)
	}
	var msgs struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatalf("messages body: %v", err)
	}
	if msgs.Model != "claude-alias" {
		t.Fatalf("client-facing model = %q, want the alias restored", msgs.Model)
	}

	// Chat-native model on its own dialect, through the suite.
	code, body = post("/suite/v1/chat/completions",
		`{"model":"chat-alias","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		t.Fatalf("chat status = %d: %s", code, body)
	}
	if path, model, _, _, _ = up.got(); path != "/v1/chat/completions" || model != "wire-chat" {
		t.Fatalf("chat upstream path=%q model=%q, want /v1/chat/completions wire-chat", path, model)
	}

	// A Messages client naming a chat-native model converts on the same path
	// the native route occupies.
	code, body = post("/suite/v1/messages",
		`{"model":"chat-alias","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, claudeHeaders)
	if code != 200 {
		t.Fatalf("converted status = %d: %s", code, body)
	}
	if path, model, _, _, _ = up.got(); path != "/v1/chat/completions" || model != "wire-chat" {
		t.Fatalf("converted upstream path=%q model=%q, want /v1/chat/completions wire-chat", path, model)
	}

	// Responses-native model on its own dialect.
	code, body = post("/suite/v1/responses", `{"model":"gpt-alias","input":"hi"}`,
		map[string]string{"Content-Type": "application/json"})
	if code != 200 {
		t.Fatalf("responses status = %d: %s", code, body)
	}
	if path, model, _, _, _ = up.got(); path != "/v1/responses" || model != "wire-resp" {
		t.Fatalf("responses upstream path=%q model=%q, want /v1/responses wire-resp", path, model)
	}

	// The client credential must not have reached the upstream verbatim: the
	// configured bearer is applied instead.
	code, body = post("/suite/v1/messages",
		`{"model":"claude-alias","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			"Content-Type":      "application/json",
			"anthropic-version": "2023-06-01",
			"Authorization":     "Bearer client-supplied-should-not-cross",
		})
	if code != 200 {
		t.Fatalf("auth status = %d: %s", code, body)
	}

	// Discovery through the suite.
	resp, err := client.Get("http://" + proxyAddr + "/suite/v1/models?limit=1000")
	if err != nil {
		t.Fatalf("discovery: %v\noutput:\n%s", err, out.String())
	}
	defer resp.Body.Close()
	disc, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("discovery status = %d: %s", resp.StatusCode, disc)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(disc, &doc); err != nil {
		t.Fatalf("discovery body: %v: %s", err, disc)
	}
	if len(doc.Data) != 3 {
		t.Fatalf("discovery listed %d models, want 3: %s", len(doc.Data), disc)
	}

	// The startup log must state the resolved routing, so an operator can see
	// what the mount will do.
	logs := out.String()
	for _, want := range []string{
		"native: 3 route(s): messages@/v1/messages, chat@/v1/chat/completions, responses@/v1/responses",
		// The upstream here is a local fake, so the preset reports the
		// off-host note rather than claiming an opencode.ai mount.
		`note: -opencode preset enabled for provider "opencode-go" whose upstream host "127.0.0.1" is not opencode.ai`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("startup log missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "client-supplied-should-not-cross") {
		t.Fatal("client credential appeared in the proxy log")
	}
}
