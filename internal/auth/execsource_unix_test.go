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

package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// credentialScript writes an executable /bin/sh script. Every run appends
// to a log first and exposes its 1-based run number as $N, so tests can
// both vary the output per run and count executions. dir is where the
// script lives, for tests that steer it with marker files.
func credentialScript(t *testing.T, body string) (path, dir string, runs func() int) {
	t.Helper()
	dir = t.TempDir()
	log := filepath.Join(dir, "runs.log")
	path = filepath.Join(dir, "credential.sh")
	script := "#!/bin/sh\n" +
		"echo run >> '" + log + "'\n" +
		"N=$(wc -l < '" + log + "' | tr -d ' ')\n" +
		"DIR='" + dir + "'\n" +
		body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runs = func() int {
		raw, err := os.ReadFile(log)
		if err != nil {
			return 0
		}
		return strings.Count(string(raw), "\n")
	}
	return path, dir, runs
}

// fakeClock is a manually advanced clock for TTL and backoff tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newScriptSource(t *testing.T, path string, ttl, timeout time.Duration) (*ExecSecretSource, *fakeClock) {
	t.Helper()
	s, err := NewExecSecretSource([]string{path}, ttl, timeout)
	if err != nil {
		t.Fatalf("NewExecSecretSource: %v", err)
	}
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s.now = clock.Now
	return s, clock
}

func mustSecret(t *testing.T, s *ExecSecretSource) string {
	t.Helper()
	v, err := s.Secret(context.Background())
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	return v
}

func TestExecSecretSourceReturnsCommandOutput(t *testing.T) {
	path, _, runs := credentialScript(t, `echo "tok-$N"`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1", got)
	}
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("cached Secret = %q, want tok-1", got)
	}
	if n := runs(); n != 1 {
		t.Fatalf("command ran %d times, want 1 (value cached within ttl)", n)
	}
}

func TestExecSecretSourceArgumentsAndStdin(t *testing.T) {
	// Arguments reach the command verbatim (no shell re-parsing) and stdin
	// is the null device, so a helper reading stdin sees EOF immediately.
	path, _, _ := credentialScript(t, `read -r line; printf '%s|%s|%s\n' "$1" "$2" "$line"`)
	s, err := NewExecSecretSource([]string{path, "a b", "$HOME"}, time.Hour, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustSecret(t, s); got != "a b|$HOME|" {
		t.Fatalf("Secret = %q, want %q", got, "a b|$HOME|")
	}
}

func TestExecSecretSourceUsesTrimmedFirstLine(t *testing.T) {
	path, _, _ := credentialScript(t, `printf '  tok-first \r\nsecond-line\nthird\n'`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-first" {
		t.Fatalf("Secret = %q, want tok-first", got)
	}
}

func TestExecSecretSourceAcceptsOutputWithoutTrailingNewline(t *testing.T) {
	path, _, _ := credentialScript(t, `printf 'tok-no-newline'`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-no-newline" {
		t.Fatalf("Secret = %q, want tok-no-newline", got)
	}
}

// assertClassError checks the error is exactly the class message and leaks
// none of the command's stdout or stderr.
func assertClassError(t *testing.T, err error, want string, leaked ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Secret: nil error, want %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err, want)
	}
	for _, l := range leaked {
		if strings.Contains(err.Error(), l) {
			t.Fatalf("error %q leaks command output %q", err, l)
		}
	}
}

func TestExecSecretSourceFailureClasses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty output", body: `exit 0`, want: "exec secret source: command produced no output"},
		{name: "blank first line", body: `printf '   \nsecret-on-line-two\n'`, want: "exec secret source: command produced no output"},
		{name: "non-zero exit", body: "echo secret-on-stdout\necho secret-on-stderr >&2\nexit 3", want: "exec secret source: command exited with status 3"},
		{name: "control characters", body: `printf 'tok\033[31mred\n'`, want: "exec secret source: command output contains control characters"},
		{name: "oversized first line", body: `head -c 70000 /dev/zero | tr '\0' 'x'`, want: "first output line exceeds 65536 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, _, _ := credentialScript(t, tc.body)
			s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
			_, err := s.Secret(context.Background())
			assertClassError(t, err, tc.want, "secret-on-stdout", "secret-on-stderr", "secret-on-line-two", "xxxx")
		})
	}
}

func TestExecSecretSourceMissingCommand(t *testing.T) {
	s, _ := newScriptSource(t, filepath.Join(t.TempDir(), "absent"), time.Hour, 10*time.Second)
	_, err := s.Secret(context.Background())
	assertClassError(t, err, "exec secret source: start command")
}

func TestExecSecretSourceTimeoutKillsCommand(t *testing.T) {
	// The background sleep shares the process group and holds stdout open:
	// killing only the shell would leave the run blocked on the pipe.
	path, _, _ := credentialScript(t, "echo partial-secret\nsleep 30 &\nsleep 30")
	s, _ := newScriptSource(t, path, time.Hour, 200*time.Millisecond)
	start := time.Now()
	_, err := s.Secret(context.Background())
	elapsed := time.Since(start)
	assertClassError(t, err, "exec secret source: command timed out after 200ms", "partial-secret")
	if elapsed > 5*time.Second {
		t.Fatalf("timed-out run took %v; the process group was not killed promptly", elapsed)
	}
}

func TestExecSecretSourceRefreshesAfterTTL(t *testing.T) {
	path, _, runs := credentialScript(t, `echo "tok-$N"`)
	s, clock := newScriptSource(t, path, 10*time.Minute, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1", got)
	}
	clock.Advance(7 * time.Minute) // before the refresh-ahead window (8m)
	if got := mustSecret(t, s); got != "tok-1" || runs() != 1 {
		t.Fatalf("Secret = %q after %d runs, want cached tok-1 after 1 run", got, runs())
	}
	clock.Advance(4 * time.Minute) // past expiry: the caller waits for a fresh value
	if got := mustSecret(t, s); got != "tok-2" {
		t.Fatalf("Secret after expiry = %q, want tok-2", got)
	}
}

func TestExecSecretSourceRefreshesAheadOfExpiryWithoutBlocking(t *testing.T) {
	path, dir, runs := credentialScript(t, `[ -f "$DIR/hold" ] && sleep 1; echo "tok-$N"`)
	s, clock := newScriptSource(t, path, 10*time.Minute, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Inside min(ttl/5, 5m) = 2m of expiry: the still-valid value is served
	// at once while the slow refresh runs in the background.
	clock.Advance(8*time.Minute + 30*time.Second)
	start := time.Now()
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret in refresh window = %q, want still-valid tok-1", got)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("Secret in refresh window blocked for %v", waited)
	}
	deadline := time.Now().Add(10 * time.Second)
	for mustSecret(t, s) != "tok-2" {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never published tok-2")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := runs(); n != 2 {
		t.Fatalf("command ran %d times, want 2 (one background refresh)", n)
	}
}

func TestExecSecretSourceCoalescesConcurrentCallers(t *testing.T) {
	path, _, runs := credentialScript(t, `sleep 0.3; echo "tok-$N"`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	const callers = 32
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			results[i], errs[i] = s.Secret(context.Background())
		})
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || results[i] != "tok-1" {
			t.Fatalf("caller %d: Secret = %q, %v; want tok-1", i, results[i], errs[i])
		}
	}
	if n := runs(); n != 1 {
		t.Fatalf("command ran %d times for %d concurrent callers, want 1", n, callers)
	}
}

func TestExecSecretSourceKeepsLastGoodValueOnRefreshFailure(t *testing.T) {
	path, dir, runs := credentialScript(t, `[ -f "$DIR/fail" ] && exit 1; echo "tok-$N"`)
	s, clock := newScriptSource(t, path, 10*time.Minute, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1", got)
	}
	failMarker := filepath.Join(dir, "fail")
	if err := os.WriteFile(failMarker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	clock.Advance(11 * time.Minute)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret after failed refresh = %q, want last good tok-1", got)
	}
	if n := runs(); n != 2 {
		t.Fatalf("runs = %d, want 2 (one failed refresh)", n)
	}
	// Within the backoff no new run starts; the last good value is served.
	if got := mustSecret(t, s); got != "tok-1" || runs() != 2 {
		t.Fatalf("Secret during backoff = %q after %d runs, want tok-1 after 2", got, runs())
	}
	// The second consecutive failure doubles the backoff.
	clock.Advance(execBackoffMin)
	if got := mustSecret(t, s); got != "tok-1" || runs() != 3 {
		t.Fatalf("Secret after first backoff = %q after %d runs, want tok-1 after 3", got, runs())
	}
	clock.Advance(execBackoffMin)
	if got := mustSecret(t, s); got != "tok-1" || runs() != 3 {
		t.Fatalf("second backoff ignored: %q after %d runs", got, runs())
	}

	if err := os.Remove(failMarker); err != nil {
		t.Fatal(err)
	}
	clock.Advance(execBackoffMin)
	if got := mustSecret(t, s); got != "tok-4" {
		t.Fatalf("Secret after recovery = %q, want tok-4", got)
	}
}

func TestExecSecretSourceErrorsOnlyWithoutAnyValue(t *testing.T) {
	path, _, runs := credentialScript(t, `exit 2`)
	s, clock := newScriptSource(t, path, time.Hour, 10*time.Second)
	_, err := s.Secret(context.Background())
	assertClassError(t, err, "command exited with status 2")
	// Within the backoff the error is returned without running again.
	_, err = s.Secret(context.Background())
	assertClassError(t, err, "command exited with status 2")
	if n := runs(); n != 1 {
		t.Fatalf("runs = %d, want 1 during backoff", n)
	}
	clock.Advance(execBackoffMin)
	if _, err := s.Secret(context.Background()); err == nil || runs() != 2 {
		t.Fatalf("after backoff: err = %v, runs = %d; want error after 2 runs", err, runs())
	}
}

func TestExecSecretSourceInvalidateForcesRefreshWithRateLimit(t *testing.T) {
	path, _, runs := credentialScript(t, `echo "tok-$N"`)
	s, clock := newScriptSource(t, path, time.Hour, 10*time.Second)
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1", got)
	}

	s.Invalidate("tok-1")
	if got := mustSecret(t, s); got != "tok-2" {
		t.Fatalf("Secret after Invalidate = %q, want tok-2", got)
	}

	// A second rejection inside the rate-limit interval does not run the
	// command again, however many requests report it.
	for range 10 {
		s.Invalidate("tok-2")
		if got := mustSecret(t, s); got != "tok-2" {
			t.Fatalf("rate-limited Secret = %q, want tok-2", got)
		}
	}
	if n := runs(); n != 2 {
		t.Fatalf("runs = %d after rate-limited invalidations, want 2", n)
	}

	clock.Advance(execInvalidateInterval)
	s.Invalidate("tok-1") // stale: tok-1 was already replaced
	if got := mustSecret(t, s); got != "tok-2" || runs() != 2 {
		t.Fatalf("stale Invalidate refreshed: %q after %d runs", got, runs())
	}
	s.Invalidate("tok-2")
	if got := mustSecret(t, s); got != "tok-3" {
		t.Fatalf("Secret after interval Invalidate = %q, want tok-3", got)
	}
}

func TestExecSecretSourceWaiterHonoursContext(t *testing.T) {
	path, _, _ := credentialScript(t, `sleep 1; echo "tok-$N"`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Secret(ctx); err == nil {
		t.Fatal("Secret: nil error for a cancelled waiter")
	}
	// The shared run was not aborted by that caller leaving.
	if got := mustSecret(t, s); got != "tok-1" {
		t.Fatalf("Secret = %q, want tok-1 from the same run", got)
	}
}

func TestFreezeAuthPolicyRetainsDynamicSource(t *testing.T) {
	path, _, runs := credentialScript(t, `echo "tok-$N"`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	frozen, err := FreezeAuthPolicy(context.Background(), &AuthPolicy{Mode: AuthBearer, Secret: s})
	if err != nil {
		t.Fatalf("FreezeAuthPolicy: %v", err)
	}
	if frozen.Secret != SecretSource(s) {
		t.Fatalf("frozen Secret = %T, want the live exec source", frozen.Secret)
	}
	if n := runs(); n != 1 {
		t.Fatalf("runs = %d, want 1 (eager resolution at freeze)", n)
	}

	req, _ := http.NewRequest(http.MethodPost, "https://upstream.example/v1/x", nil)
	if err := ApplyUpstreamAuthentication(context.Background(), req, frozen); err != nil {
		t.Fatalf("ApplyUpstreamAuthentication: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want Bearer tok-1", got)
	}
	reauth, ok := ReauthenticationFromContext(req.Context())
	if !ok {
		t.Fatal("request authenticated by a dynamic source carries no reauthentication marker")
	}
	if reauth.Source != DynamicSecretSource(s) || reauth.Secret != "tok-1" ||
		reauth.Header != "Authorization" || reauth.Scheme != "Bearer " {
		t.Fatalf("marker = %+v, want the exec source, tok-1, Authorization, Bearer", reauth)
	}
}

func TestFreezeAuthPolicyFailsOnBrokenDynamicSource(t *testing.T) {
	path, _, _ := credentialScript(t, `exit 1`)
	s, _ := newScriptSource(t, path, time.Hour, 10*time.Second)
	if _, err := FreezeAuthPolicy(context.Background(), &AuthPolicy{Mode: AuthBearer, Secret: s}); err == nil {
		t.Fatal("FreezeAuthPolicy: nil error for a failing exec source")
	}
}

func TestStaticSourceAttachesNoReauthentication(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://upstream.example/v1/x", nil)
	policy := &AuthPolicy{Mode: AuthXAPIKey, Secret: NewStaticSecretSource("k"), AnthropicVersion: DefaultAnthropicVersion}
	if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReauthenticationFromContext(req.Context()); ok {
		t.Fatal("static source attached a reauthentication marker")
	}
	if got := req.Header.Get("X-Api-Key"); got != "k" {
		t.Fatalf("X-Api-Key = %q, want k", got)
	}
}
