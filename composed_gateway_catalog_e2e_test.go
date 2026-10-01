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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// launcherMountPrefix mirrors the ecosystem launcher's mount derivation
// (catalog.js mountPrefix): the first declared endpoint's path with a trailing
// /v1 removed and trailing slashes trimmed.
func launcherMountPrefix(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	return strings.TrimSuffix(path, "/v1")
}

// launcherShaperEnvVar mirrors the launcher's credential variable derivation
// (catalog.js shaperEnvVar): SHAPER_PROVIDER_<requiredEnv[0] without a trailing
// _API_KEY>_API_KEY.
func launcherShaperEnvVar(requiredEnv []string) string {
	declared := ""
	if len(requiredEnv) > 0 {
		declared = requiredEnv[0]
	}
	return "SHAPER_PROVIDER_" + strings.TrimSuffix(declared, "_API_KEY") + "_API_KEY"
}

// generatedAccess is one launcher-produced shaper mount.
type generatedAccess struct {
	provider    string
	endpoint    string
	upstream    string
	requiredEnv []string
	transcode   []string
	models      []generatedModel
}

// generatedModel is one model-table entry the launcher emits for a mount.
type generatedModel struct {
	surrogate string
	wire      string
	facts     string
}

// generatedArgv produces the argv a launcher emits for the given accesses: the
// global model-table entries at server scope (before the first marker), then one
// provider section per access with its prefix and credential derived by the same
// rules the launcher uses. It also returns the child environment entries.
func generatedArgv(accesses []generatedAccess) ([]string, []string) {
	var tableArgs []string
	var sectionArgs []string
	var env []string
	for _, access := range accesses {
		for _, model := range access.models {
			tableArgs = append(tableArgs, "-model-table",
				model.surrogate+"@"+access.provider+"="+model.wire+model.facts)
		}
		envVar := launcherShaperEnvVar(access.requiredEnv)
		sectionArgs = append(sectionArgs, "--provider="+access.provider,
			"-upstream", access.upstream,
			"-prefix", launcherMountPrefix(access.endpoint),
			"-auth-source", "env:"+envVar,
			"-auth-mode", "bearer",
		)
		sectionArgs = append(sectionArgs, access.transcode...)
		env = append(env, envVar+"=secret-"+access.provider)
	}
	return append(tableArgs, sectionArgs...), env
}

// TestE2E_ComposedGateway_ModelCatalog proves the generated-argv integration
// surface end to end: a launcher-shaped command line starts the binary, the
// per-mount catalogs list exactly each mount's declared surrogates, declared
// models resolve to their wire ids upstream with the mount's own credential,
// undeclared models are refused with the servable set, the listing never
// touches an upstream, and a held completion does not delay discovery.
func TestE2E_ComposedGateway_ModelCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bin := t.TempDir() + "/catalog-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var (
		dialagramHits atomic.Int64
		dialagramAuth atomic.Value
		dialagramBody atomic.Value
		dialagramGate = make(chan struct{})
		verbooHits    atomic.Int64
		verbooAuth    atomic.Value
		verbooBody    atomic.Value
	)
	chatResponse := func(model string) string {
		return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"` + model + `",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	}
	upstreamDialagram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		dialagramHits.Add(1)
		dialagramAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		dialagramBody.Store(string(body))
		if strings.Contains(string(body), `"wire-slow"`) {
			<-dialagramGate
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatResponse("wire")))
	}))
	t.Cleanup(upstreamDialagram.Close)
	upstreamVerboo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		verbooHits.Add(1)
		verbooAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		verbooBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatResponse("wire")))
	}))
	t.Cleanup(upstreamVerboo.Close)

	accesses := []generatedAccess{
		{
			provider:    "dialagram",
			endpoint:    "http://127.0.0.1:11239/dialagram/v1",
			upstream:    upstreamDialagram.URL,
			requiredEnv: []string{"DIALAGRAM_API_KEY"},
			transcode:   []string{"-transcode-responses-chat", "-limit", "POST /v1/responses:1"},
			models: []generatedModel{
				{surrogate: "qwen-max", wire: "wire-qwen", facts: ";context=32000;max_output=8192"},
				{surrogate: "qwen-slow", wire: "wire-slow"},
			},
		},
		{
			provider:    "verboo",
			endpoint:    "http://127.0.0.1:11239/verboo",
			upstream:    upstreamVerboo.URL,
			requiredEnv: []string{"VERBOO_API_KEY"},
			transcode:   []string{"-transcode-messages-chat"},
			models: []generatedModel{
				{surrogate: "glm-5.3-flash", wire: "wire-glm", facts: ";context=200000;efforts=low+high"},
			},
		},
	}
	generated, generatedEnv := generatedArgv(accesses)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	args := append([]string{"-bind", addr}, generated...)
	var out safeBuffer
	cmd := exec.Command(bin, args...)
	var filteredEnv []string
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "SHAPER_PROVIDER_") {
			filteredEnv = append(filteredEnv, env)
		}
	}
	cmd.Env = append(filteredEnv, generatedEnv...)
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}()
	if err := waitTCPReady(addr, 5*time.Second); err != nil {
		t.Fatalf("gateway not ready: %v\n%s", err, out.String())
	}
	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path string, headers map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	post := func(path, payload string) (int, string) {
		resp, err := client.Post("http://"+addr+path, "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// (a) Codex discovery on the Responses mount, then resolution through it.
	status, body := get("/dialagram/v1/models?client_version=1", nil)
	if status != http.StatusOK {
		t.Fatalf("dialagram catalog status = %d: %s", status, body)
	}
	for _, want := range []string{`"slug":"qwen-max"`, `"slug":"qwen-slow"`, `"context_window":32000`} {
		if !strings.Contains(body, want) {
			t.Errorf("dialagram catalog missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "wire-qwen") || strings.Contains(body, `"glm-5.3-flash"`) {
		t.Errorf("dialagram catalog leaks a wire id or another mount's model: %s", body)
	}
	if dialagramHits.Load() != 0 || verbooHits.Load() != 0 {
		t.Fatalf("catalog touched an upstream: %d/%d", dialagramHits.Load(), verbooHits.Load())
	}

	status, body = post("/dialagram/v1/responses", `{"model":"qwen-max","input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("resolve status = %d: %s", status, body)
	}
	if got := dialagramBody.Load().(string); !strings.Contains(got, `"model":"wire-qwen"`) {
		t.Errorf("upstream body did not carry the wire id: %s", got)
	}
	if !strings.Contains(body, `"model":"qwen-max"`) {
		t.Errorf("downstream did not return the surrogate: %s", body)
	}
	if got := dialagramAuth.Load().(string); got != "Bearer secret-dialagram" {
		t.Errorf("dialagram upstream credential = %q, want its own mount secret", got)
	}

	// (b) Anthropic discovery on the Messages mount, then resolution.
	status, body = get("/verboo/v1/models", map[string]string{"Anthropic-Version": "2023-06-01"})
	if status != http.StatusOK {
		t.Fatalf("verboo catalog status = %d: %s", status, body)
	}
	if !strings.Contains(body, `"id":"glm-5.3-flash"`) || strings.Contains(body, "wire-glm") {
		t.Errorf("verboo catalog body = %s", body)
	}
	if strings.Contains(body, "qwen-max") {
		t.Errorf("verboo catalog leaked another mount's model: %s", body)
	}
	status, body = post("/verboo/v1/messages", `{"model":"glm-5.3-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("verboo resolve status = %d: %s", status, body)
	}
	if got := verbooBody.Load().(string); !strings.Contains(got, `"model":"wire-glm"`) {
		t.Errorf("verboo upstream body did not carry the wire id: %s", got)
	}
	if got := verbooAuth.Load().(string); got != "Bearer secret-verboo" {
		t.Errorf("verboo upstream credential = %q, want its own mount secret", got)
	}

	// (c) An undeclared model is refused with the mount's servable set.
	status, body = post("/dialagram/v1/responses", `{"model":"undeclared","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("undeclared status = %d: %s", status, body)
	}
	for _, want := range []string{"servable on this mount", "qwen-max", "qwen-slow"} {
		if !strings.Contains(body, want) {
			t.Errorf("undeclared error missing %q: %s", want, body)
		}
	}

	// (d) A held completion must not delay discovery.
	held := make(chan int, 1)
	go func() {
		status, _ := post("/dialagram/v1/responses", `{"model":"qwen-slow","input":"hold"}`)
		held <- status
	}()
	deadline := time.Now().Add(5 * time.Second)
	for dialagramHits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	status, body = get("/dialagram/v1/models?client_version=1", nil)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("catalog timed out behind the held completion after %v", elapsed)
	}
	if status != http.StatusOK {
		t.Fatalf("catalog behind a held slot = %d: %s", status, body)
	}
	close(dialagramGate)
	if status := <-held; status != http.StatusOK {
		t.Fatalf("held completion status = %d", status)
	}

	if dialagramHits.Load() != 2 || verbooHits.Load() != 1 {
		t.Errorf("unexpected upstream hit counts: dialagram=%d verboo=%d", dialagramHits.Load(), verbooHits.Load())
	}
	if !strings.Contains(out.String(), "model table: 3 surrogates across 2 providers:") {
		t.Errorf("startup log lacks the model-table summary:\n%s", out.String())
	}
}

// TestE2E_ComposedGateway_LegacyModelMapEquivalence proves the documented
// migration: a legacy per-provider -transcode-model configuration and its
// -model-table equivalent serve byte-identical exchanges, and a configuration
// carrying both fails startup loudly.
func TestE2E_ComposedGateway_LegacyModelMapEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bin := t.TempDir() + "/migration-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	var upstreamBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"wire-m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	run := func(args []string) (string, string) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		var out safeBuffer
		cmd := exec.Command(bin, append([]string{"-bind", addr}, args...)...)
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				_ = cmd.Wait()
			}
		}()
		if err := waitTCPReady(addr, 5*time.Second); err != nil {
			t.Fatalf("gateway not ready: %v\n%s", err, out.String())
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post("http://"+addr+"/openai/v1/responses", "application/json",
			strings.NewReader(`{"model":"m","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		downstream, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, downstream)
		}
		return upstreamBody.Load().(string), string(downstream)
	}

	normalize := func(s string) string {
		s = regexp.MustCompile(`(resp|msg)_[0-9a-zA-Z]+`).ReplaceAllString(s, "$1_GENERATED")
		s = regexp.MustCompile(`"created_at":\d+`).ReplaceAllString(s, `"created_at":0`)
		return s
	}

	legacyUpstream, legacyDownstream := run([]string{
		"--provider=openai",
		"-upstream", upstream.URL,
		"-prefix", "/openai",
		"-transcode-responses-chat",
		"-transcode-model", "m=wire-m",
	})
	tableUpstream, tableDownstream := run([]string{
		"-model-table", "m@openai=wire-m",
		"--provider=openai",
		"-upstream", upstream.URL,
		"-prefix", "/openai",
		"-transcode-responses-chat",
	})

	if legacyUpstream != tableUpstream {
		t.Errorf("upstream request differs across the migration:\nlegacy: %s\ntable:  %s", legacyUpstream, tableUpstream)
	}
	if normalize(legacyDownstream) != normalize(tableDownstream) {
		t.Errorf("downstream response differs across the migration:\nlegacy: %s\ntable:  %s", legacyDownstream, tableDownstream)
	}
	if !strings.Contains(legacyDownstream, `"model":"m"`) || !strings.Contains(tableDownstream, `"model":"m"`) {
		t.Errorf("the client-visible alias changed: %s / %s", legacyDownstream, tableDownstream)
	}

	// A mixed configuration must fail startup, naming both flags and the provider.
	mixed := exec.Command(bin,
		"-model-table", "m@openai=wire-m",
		"--provider=openai",
		"-upstream", upstream.URL,
		"-prefix", "/openai",
		"-transcode-responses-chat",
		"-transcode-model", "m=wire-m",
	)
	out, err := mixed.CombinedOutput()
	if err == nil {
		t.Fatalf("mixed configuration started; want startup failure:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Errorf("mixed configuration error = %v, want exit 1", err)
	}
	for _, want := range []string{"-model-table and -transcode-model cannot be combined", `provider "openai"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("mixed startup output missing %q:\n%s", want, out)
		}
	}
}
