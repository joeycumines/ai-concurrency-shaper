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

//go:build unix

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// rotatingCredential returns an exec source whose command prints tok-A on
// its first run and tok-B on every later run (an expired key followed by
// its replacement), plus a run counter.
func rotatingCredential(t *testing.T) (*auth.ExecSecretSource, func() int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "runs.log")
	path := filepath.Join(dir, "credential.sh")
	script := "#!/bin/sh\n" +
		"echo run >> '" + log + "'\n" +
		"N=$(wc -l < '" + log + "' | tr -d ' ')\n" +
		`if [ "$N" -eq 1 ]; then echo tok-A; else echo tok-B; fi` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := auth.NewExecSecretSource([]string{path}, time.Hour, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return source, func() int {
		raw, _ := os.ReadFile(log)
		return strings.Count(string(raw), "\n")
	}
}

// upstreamHit is one request observed by tokenUpstream.
type upstreamHit struct {
	path, authorization, body string
}

// tokenUpstream answers 401 unless the request carries Bearer tok-B, and
// records every request it receives.
type tokenUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	hits []upstreamHit
}

func newTokenUpstream(t *testing.T, okBody string) *tokenUpstream {
	t.Helper()
	u := &tokenUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits = append(u.hits, upstreamHit{path: r.URL.Path, authorization: r.Header.Get("Authorization"), body: string(body)})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer tok-B" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"expired credential","type":"invalid_request_error"}}`)
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *tokenUpstream) Hits() []upstreamHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamHit(nil), u.hits...)
}

func (u *tokenUpstream) URL(t *testing.T) *url.URL {
	t.Helper()
	parsed, err := url.Parse(u.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// serveProxy fronts p with a real server so inbound request bodies are
// server-side streams without GetBody, exactly as clients deliver them.
func serveProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front.URL
}

func postJSON(t *testing.T, target, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	// An opaque body (no GetBody, unknown length) keeps the client from
	// replaying it: any replay must come from the proxy.
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const chatCompletionOK = `{"id":"c1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

func newExecAuthProxy(t *testing.T, upstream *url.URL, secret auth.SecretSource, retries int, mappings ...TranscodeMapping) *Proxy {
	t.Helper()
	opts := []Option{
		WithUpstream(upstream),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(&auth.AuthPolicy{Mode: auth.AuthBearer, Secret: secret}),
		WithMaxRetries(retries),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
		WithRetryMinDelay(0),
	}
	if retries != 0 {
		opts = append(opts, WithMaxBodyBytes(1<<20))
	}
	if len(mappings) > 0 {
		opts = append(opts, WithTranscodeMapping(mappings...))
	}
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestProxyExecCredentialRejectionReauthenticatesPassthrough(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	upstream := newTokenUpstream(t, chatCompletionOK)
	source, runs := rotatingCredential(t)
	p := newExecAuthProxy(t, upstream.URL(t), source, 2)
	front := serveProxy(t, p)

	status, got := postJSON(t, front+"/v1/chat/completions", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200 after reauthentication", status, got)
	}
	hits := upstream.Hits()
	if len(hits) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (rejected + one re-send): %+v", len(hits), hits)
	}
	if hits[0].authorization != "Bearer tok-A" || hits[1].authorization != "Bearer tok-B" {
		t.Fatalf("credentials sent = %q then %q, want tok-A then tok-B", hits[0].authorization, hits[1].authorization)
	}
	if hits[1].body != body {
		t.Fatalf("re-sent body = %q, want the original %q", hits[1].body, body)
	}
	if n := runs(); n != 2 {
		t.Fatalf("credential command ran %d times, want 2 (startup + one forced refresh)", n)
	}

	// The next request uses the refreshed credential directly.
	if status, got := postJSON(t, front+"/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("follow-up status = %d, body %s", status, got)
	}
	if hits := upstream.Hits(); len(hits) != 3 || hits[2].authorization != "Bearer tok-B" {
		t.Fatalf("follow-up hits = %+v, want a third request with tok-B", hits)
	}
}

func TestProxyExecCredentialRejectionWithoutReplayableBodyOnlyInvalidates(t *testing.T) {
	// Retries disabled: the passthrough body is a one-shot stream, so the
	// rejection is returned, but the credential is refreshed for the next
	// request.
	const body = `{"model":"m"}`
	upstream := newTokenUpstream(t, chatCompletionOK)
	source, runs := rotatingCredential(t)
	p := newExecAuthProxy(t, upstream.URL(t), source, 0)
	front := serveProxy(t, p)

	if status, _ := postJSON(t, front+"/v1/chat/completions", body); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 (body not replayable)", status)
	}
	if hits := upstream.Hits(); len(hits) != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (no re-send)", len(hits))
	}
	if status, got := postJSON(t, front+"/v1/chat/completions", body); status != http.StatusOK {
		t.Fatalf("next request status = %d, body %s; want 200 with the refreshed credential", status, got)
	}
	if n := runs(); n != 2 {
		t.Fatalf("credential command ran %d times, want 2 (the rejection forced one refresh)", n)
	}
}

func TestProxyExecCredentialRejectionReauthenticatesMessagesToChat(t *testing.T) {
	upstream := newTokenUpstream(t, chatCompletionOK)
	source, runs := rotatingCredential(t)
	clientRoute, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      clientRoute,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
		}},
		ModelMap: transcode.ModelMap{AllowIdentity: true},
		// Auth left zero: the mapping inherits the provider policy.
	}}
	// Retries disabled: the converted request is rebuilt from memory, so
	// the transcode path is replayable without the retry buffer.
	p := newExecAuthProxy(t, upstream.URL(t), source, 0, mapping)
	front := serveProxy(t, p)

	status, got := postJSON(t, front+"/v1/messages", `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200 after reauthentication", status, got)
	}
	if !strings.Contains(got, `"type":"message"`) || !strings.Contains(got, "hello") {
		t.Fatalf("response is not the converted Messages reply: %s", got)
	}
	hits := upstream.Hits()
	if len(hits) != 2 || hits[0].path != "/v1/chat/completions" ||
		hits[0].authorization != "Bearer tok-A" || hits[1].authorization != "Bearer tok-B" {
		t.Fatalf("upstream hits = %+v, want tok-A rejected then tok-B on /v1/chat/completions", hits)
	}
	if hits[0].body != hits[1].body {
		t.Fatalf("re-sent converted body differs:\n%s\n%s", hits[0].body, hits[1].body)
	}
	if n := runs(); n != 2 {
		t.Fatalf("credential command ran %d times, want 2", n)
	}
}

func TestProxyStaticCredentialRejectionIsNotRetried(t *testing.T) {
	upstream := newTokenUpstream(t, chatCompletionOK)
	p := newExecAuthProxy(t, upstream.URL(t), auth.NewStaticSecretSource("tok-A"), 3)
	front := serveProxy(t, p)

	if status, _ := postJSON(t, front+"/v1/chat/completions", `{"model":"m"}`); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401", status)
	}
	if hits := upstream.Hits(); len(hits) != 1 {
		t.Fatalf("upstream saw %d requests for a static credential, want 1 (401 is never retried)", len(hits))
	}
}

func TestProxyExecCredentialRateLimitBanIsNotReauthenticated(t *testing.T) {
	// A 403 carrying rate-limit signals is a temporary ban, not a credential
	// rejection: no invalidation, no re-send.
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	source, runs := rotatingCredential(t)
	p := newExecAuthProxy(t, upstreamURL, source, 0)
	front := serveProxy(t, p)

	if status, _ := postJSON(t, front+"/v1/chat/completions", `{}`); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 || runs() != 1 {
		t.Fatalf("hits = %d, credential runs = %d; want 1 and 1 (ban is not a credential failure)", hits, runs())
	}
}

func TestProxyExecCredentialRetriesAfterReauthenticationUseFreshCredential(t *testing.T) {
	// The fresh credential's first attempt hits a transient 503: the retry
	// that follows must carry the fresh credential, not the rejected one.
	var mu sync.Mutex
	var seen []string
	freshAttempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		defer mu.Unlock()
		authz := r.Header.Get("Authorization")
		seen = append(seen, authz)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case authz != "Bearer tok-B":
			w.WriteHeader(http.StatusUnauthorized)
		case freshAttempts == 0:
			freshAttempts++
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, chatCompletionOK)
		}
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	source, _ := rotatingCredential(t)
	p := newExecAuthProxy(t, upstreamURL, source, 2)
	front := serveProxy(t, p)

	if status, got := postJSON(t, front+"/v1/chat/completions", `{"model":"m"}`); status != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", status, got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer tok-A", "Bearer tok-B", "Bearer tok-B"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("credentials sent = %v, want %v", seen, want)
	}
}
