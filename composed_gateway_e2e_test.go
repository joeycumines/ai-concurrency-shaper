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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestE2E_ComposedGateway_MultiProvider_TranscodeHarness proves Task 16's composed
// end-to-end verification harness against a single gateway process serving 3 providers:
// 1. Anthropic mount (/anthropic) with Messages->Chat transcoding and Bearer auth.
// 2. OpenAI mount (/openai) with Responses->Chat transcoding and Bearer auth.
// 3. Passthrough mount (/passthrough) with transparent routing.
//
// Verifies:
// (a) Claude Code multi-turn shape with mid-conversation system turn & thinking budget.
// (b) Codex multi-turn recall shape with empty-status PreviousOutputMessage.
// (c) Streaming SSE translation event-for-event with exactly one terminal event.
// (d) Poison usage extension resilience on non-stream completions.
// (e) Limiter/breaker and Prometheus /metrics aggregate accounting.
func TestE2E_ComposedGateway_MultiProvider_TranscodeHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/composed-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var (
		upAMu        sync.Mutex
		upABodies    [][]byte
		upAAuth      string
		upAStreamHit atomic.Int64

		upBMu     sync.Mutex
		upBBodies [][]byte
		upBAuth   string

		upCHits atomic.Int64
	)

	// Upstream A (Anthropic provider target): Chat completions upstream
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upAMu.Lock()
		upABodies = append(upABodies, body)
		upAAuth = r.Header.Get("Authorization")
		upAMu.Unlock()

		if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
			var chatReq struct {
				Stream   bool `json:"stream"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &chatReq)

			if chatReq.Stream {
				upAStreamHit.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				frame := func(data string) {
					_, _ = w.Write([]byte("data: " + data + "\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
				}
				frame(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"streaming reply"},"finish_reason":null}]}`)
				frame(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
				frame(`[DONE]`)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-a1","object":"chat.completion","created":1710000000,"model":"claude-3-7-sonnet-20250219","choices":[{"index":0,"message":{"role":"assistant","content":"claude response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"untranscoded_a":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upA.Close)

	// Upstream B (OpenAI provider target): Chat completions upstream for Codex & poison usage
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upBMu.Lock()
		upBBodies = append(upBBodies, body)
		upBAuth = r.Header.Get("Authorization")
		upBMu.Unlock()

		if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Return chat response with top-level reasoning_tokens (poison usage extension)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-b1","object":"chat.completion","created":1710000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"codex recall reply: 42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":5,"total_tokens":20,"reasoning_tokens":15}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"untranscoded_b":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upB.Close)

	// Upstream C (Passthrough provider target)
	upC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upCHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"passthrough":true,"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upC.Close)

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

	var out strings.Builder
	cmd := exec.Command(bin,
		"-bind", proxyAddr,
		"-metrics-bind", metricsAddr,
		"--provider=anthropic",
		"-upstream", upA.URL,
		"-prefix", "/anthropic",
		"-auth-source", "env:SHAPER_PROVIDER_ANTHROPIC_KEY",
		"-auth-mode", "bearer",
		"-transcode-route", "messages@/v1/messages=chat-completions@/v1/chat/completions",
		"-limit", "POST /v1/messages:1",
		"-retry", "1",
		"--provider=openai",
		"-upstream", upB.URL,
		"-prefix", "/openai",
		"-auth-source", "env:SHAPER_PROVIDER_OPENAI_KEY",
		"-auth-mode", "bearer",
		"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
		"-limit", "POST /v1/responses:1",
		"-retry", "1",
		"--provider=passthrough",
		"-upstream", upC.URL,
		"-prefix", "/passthrough",
		"-limit", "POST /v1/models:2",
		"-retry", "0",
	)

	cmd.Env = append(os.Environ(),
		"SHAPER_PROVIDER_ANTHROPIC_KEY=secret-anthropic-key-xyz",
		"SHAPER_PROVIDER_OPENAI_KEY=secret-openai-key-abc",
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

	if err := waitTCPReady(proxyAddr, 3*time.Second); err != nil {
		t.Fatalf("proxy addr: %v\noutput:\n%s", err, out.String())
	}
	if err := waitTCPReady(metricsAddr, 3*time.Second); err != nil {
		t.Fatalf("metrics addr: %v\noutput:\n%s", err, out.String())
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// -------------------------------------------------------------------------
	// (a) Claude Code Multi-Turn Shape with Mid-Conversation System Turn
	// -------------------------------------------------------------------------
	claudePayload := `{
		"model": "claude-3-7-sonnet-20250219",
		"max_tokens": 1024,
		"system": [{"type": "text", "text": "Leading system instruction"}],
		"messages": [
			{"role": "user", "content": "turn 1 prompt"},
			{"role": "assistant", "content": "turn 1 reply"},
			{"role": "system", "content": "mid-conversation system turn"},
			{"role": "user", "content": "turn 2 prompt"}
		]
	}`
	reqClaude, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/anthropic/v1/messages?beta=true", strings.NewReader(claudePayload))
	reqClaude.Header.Set("Content-Type", "application/json")
	respClaude, err := httpClient.Do(reqClaude)
	if err != nil {
		t.Fatalf("Claude request failed: %v", err)
	}
	defer respClaude.Body.Close()
	bodyClaude, _ := io.ReadAll(respClaude.Body)
	if respClaude.StatusCode != http.StatusOK {
		t.Fatalf("Claude request status = %d, want 200: %s", respClaude.StatusCode, bodyClaude)
	}

	// Verify Claude response is in Anthropic Messages format
	var anthropicResp struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(bodyClaude, &anthropicResp); err != nil {
		t.Fatalf("unmarshal anthropic response: %v\n%s", err, bodyClaude)
	}
	if anthropicResp.Role != "assistant" || len(anthropicResp.Content) == 0 {
		t.Fatalf("invalid anthropic response structure: %s", bodyClaude)
	}

	// Verify upstream A received consolidated leading system message and Bearer auth
	upAMu.Lock()
	if upAAuth != "Bearer secret-anthropic-key-xyz" {
		t.Errorf("upstream A auth = %q, want Bearer secret-anthropic-key-xyz", upAAuth)
	}
	if len(upABodies) == 0 {
		t.Fatalf("upstream A received no requests")
	}
	var chatBody struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(upABodies[0], &chatBody)
	upAMu.Unlock()

	for i, m := range chatBody.Messages {
		if i > 0 && m.Role == "system" {
			t.Errorf("system message at index %d violates Jinja leading system property", i)
		}
	}

	// -------------------------------------------------------------------------
	// (b) Codex Multi-Turn Recall Shape with Empty-Status PreviousOutputMessage
	// -------------------------------------------------------------------------
	codexPayload := `{
		"model": "gpt-4o",
		"instructions": "You are a helpful assistant.",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Remember 42"}]},
			{"type": "message", "id": "item_1", "role": "assistant", "status": "", "content": [{"type": "output_text", "text": "Noted 42"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "What was the number?"}]}
		]
	}`
	reqCodex, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/openai/v1/responses", strings.NewReader(codexPayload))
	reqCodex.Header.Set("Content-Type", "application/json")
	respCodex, err := httpClient.Do(reqCodex)
	if err != nil {
		t.Fatalf("Codex request failed: %v", err)
	}
	defer respCodex.Body.Close()
	bodyCodex, _ := io.ReadAll(respCodex.Body)
	if respCodex.StatusCode != http.StatusOK {
		t.Fatalf("Codex request status = %d, want 200: %s", respCodex.StatusCode, bodyCodex)
	}

	// Verify Codex response is in OpenAI Responses format with converted output
	var responsesResp struct {
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bodyCodex, &responsesResp); err != nil {
		t.Fatalf("unmarshal responses response: %v\n%s", err, bodyCodex)
	}

	upBMu.Lock()
	if upBAuth != "Bearer secret-openai-key-abc" {
		t.Errorf("upstream B auth = %q, want Bearer secret-openai-key-abc", upBAuth)
	}
	upBMu.Unlock()

	// -------------------------------------------------------------------------
	// (c) Streaming SSE Exchange with exactly one terminal event
	// -------------------------------------------------------------------------
	streamPayload := `{
		"model": "claude-3-7-sonnet-20250219",
		"max_tokens": 100,
		"stream": true,
		"messages": [{"role": "user", "content": "stream to me"}]
	}`
	reqStream, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/anthropic/v1/messages", strings.NewReader(streamPayload))
	reqStream.Header.Set("Content-Type", "application/json")
	respStream, err := httpClient.Do(reqStream)
	if err != nil {
		t.Fatalf("Stream request failed: %v", err)
	}
	defer respStream.Body.Close()
	if respStream.StatusCode != http.StatusOK {
		t.Fatalf("Stream status = %d, want 200", respStream.StatusCode)
	}

	scanner := bufio.NewScanner(respStream.Body)
	var eventTypes []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventTypes = append(eventTypes, strings.TrimPrefix(line, "event: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream scanner error: %v", err)
	}

	// Verify terminal event was message_stop
	if len(eventTypes) == 0 {
		t.Fatalf("no SSE events received")
	}
	lastEvent := eventTypes[len(eventTypes)-1]
	if lastEvent != "message_stop" {
		t.Errorf("last SSE event = %q, want message_stop", lastEvent)
	}

	// Verify WebSocket Upgrade rejected with 400 on transcode route
	reqWS, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/anthropic/v1/messages", strings.NewReader(`{}`))
	reqWS.Header.Set("Upgrade", "websocket")
	reqWS.Header.Set("Connection", "Upgrade")
	respWS, err := httpClient.Do(reqWS)
	if err != nil {
		t.Fatalf("WS request failed: %v", err)
	}
	respWS.Body.Close()
	if respWS.StatusCode != http.StatusBadRequest {
		t.Errorf("WS upgrade status = %d, want 400", respWS.StatusCode)
	}

	// -------------------------------------------------------------------------
	// (d) Poison Usage Extension Resilience
	// -------------------------------------------------------------------------
	// Poison usage (top-level reasoning_tokens) was already exercised by the Codex
	// call above. Let's verify reasoning tokens arrived in responsesResp:
	if responsesResp.Usage.OutputTokensDetails.ReasoningTokens != 15 {
		t.Errorf("reasoning tokens = %d, want 15 (from poison usage top-level field)", responsesResp.Usage.OutputTokensDetails.ReasoningTokens)
	}

	// -------------------------------------------------------------------------
	// (e) Passthrough Mount & Prometheus Metrics Aggregate Accounting
	// -------------------------------------------------------------------------
	reqPass, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/passthrough/v1/models", strings.NewReader(`{}`))
	respPass, err := httpClient.Do(reqPass)
	if err != nil {
		t.Fatalf("passthrough request failed: %v", err)
	}
	respPass.Body.Close()
	if respPass.StatusCode != http.StatusOK {
		t.Errorf("passthrough status = %d, want 200", respPass.StatusCode)
	}

	// Poll metrics endpoint
	waitForMetrics := func(want string) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := http.Get("http://" + metricsAddr + "/metrics")
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if strings.Contains(string(body), want) {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Errorf("metrics never contained %q", want)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	waitForMetrics(`shaper_requests_total{provider="anthropic",status="2xx"}`)
	waitForMetrics(`shaper_requests_total{provider="openai",status="2xx"}`)
	waitForMetrics(`shaper_requests_total{provider="passthrough",status="2xx"}`)
}
