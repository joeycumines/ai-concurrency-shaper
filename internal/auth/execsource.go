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

package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// DynamicSecretSource is a SecretSource whose value legitimately changes over
// the life of the process (short-lived credentials). Freezing a policy that
// carries one resolves it eagerly, so a broken source still fails at
// startup, but retains the source instead of pinning the first value.
type DynamicSecretSource interface {
	SecretSource

	// Invalidate reports that rejected, a value previously returned by
	// Secret, was refused by the upstream. When rejected is still the
	// current value the next Secret call re-acquires the credential,
	// subject to the source's own rate limit; a stale rejection (the value
	// has already been replaced) is ignored.
	Invalidate(rejected string)
}

const (
	// execOutputLimit bounds how much command output is retained. The
	// credential is the first line, so anything past the bound is drained
	// and discarded (never blocking the child on a full pipe).
	execOutputLimit = 64 << 10
	// execRefreshAheadCap caps the refresh-ahead window: the credential is
	// refreshed once it is within min(ttl/5, 5m) of expiry.
	execRefreshAheadCap = 5 * time.Minute
	// execBackoffMin and execBackoffMax bound the delay between attempts
	// after a failed refresh (doubling from min to max).
	execBackoffMin = time.Second
	execBackoffMax = time.Minute
	// execInvalidateInterval is the minimum spacing between forced
	// refreshes, so a burst of upstream 401s cannot fork a process each.
	execInvalidateInterval = 5 * time.Second
	// execWaitDelay bounds how long the output pipe may stay open after the
	// command exits or is killed (a backgrounded grandchild holding stdout).
	execWaitDelay = time.Second
)

// ExecSecretSource obtains a short-lived credential by running a command and
// caches it for a fixed TTL measured from the start of the acquiring run.
//
// The command runs without a shell, with stdin attached to the null device,
// the process environment inherited, and stderr discarded; the credential is
// the first line of stdout, trimmed. Errors describe the failure class only
// and never include command output.
//
// Refreshes are request-driven: inside the refresh-ahead window a caller
// triggers a background refresh and is served the still-valid value, so a
// steadily used source never blocks on the command. A caller only waits when
// the value has expired or was invalidated. No timer runs while the proxy is
// idle, so the command is never executed unless a request needs it. All
// callers share one in-flight run, and a failed refresh keeps serving the
// previous value while retrying with bounded exponential backoff.
type ExecSecretSource struct {
	argv    []string
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu sync.Mutex
	// value is the last successfully acquired credential ("" before the
	// first success).
	value string
	// expires is when value stops being served without a blocking refresh;
	// refreshAt is when a background refresh starts.
	expires   time.Time
	refreshAt time.Time
	// forced marks value as rejected upstream: the next caller waits for a
	// refresh. lastForced rate-limits how often that can happen.
	forced     bool
	lastForced time.Time
	// retryAt gates new runs after a failure; failures sizes the backoff.
	retryAt  time.Time
	failures int
	lastErr  error
	inflight *execRun
}

// execRun is one in-flight command execution shared by every waiter.
type execRun struct{ done chan struct{} }

// NewExecSecretSource validates the command and durations and returns a
// source that has not run yet: the first Secret call executes the command.
// argv[0] must be an absolute path or a bare name resolved through PATH; a
// relative path would depend on the working directory and is rejected.
func NewExecSecretSource(argv []string, ttl, timeout time.Duration) (*ExecSecretSource, error) {
	if len(argv) == 0 {
		return nil, errors.New("exec secret source: command must not be empty")
	}
	for i, arg := range argv {
		if arg == "" {
			return nil, fmt.Errorf("exec secret source: argument %d is empty", i)
		}
	}
	if !filepath.IsAbs(argv[0]) && filepath.Base(argv[0]) != argv[0] {
		return nil, fmt.Errorf("exec secret source: command %q must be an absolute path or a name resolved through PATH", argv[0])
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("exec secret source: ttl must be > 0, got %v", ttl)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("exec secret source: timeout must be > 0, got %v", timeout)
	}
	return &ExecSecretSource{
		argv:    append([]string(nil), argv...),
		ttl:     ttl,
		timeout: timeout,
		now:     time.Now,
	}, nil
}

// Secret returns the cached credential, refreshing it as described on the
// type. It fails only when no credential has ever been acquired.
func (s *ExecSecretSource) Secret(ctx context.Context) (string, error) {
	s.mu.Lock()
	now := s.now()
	if s.value != "" && !s.forced && now.Before(s.expires) {
		if !now.Before(s.refreshAt) {
			s.startLocked(now)
		}
		value := s.value
		s.mu.Unlock()
		return value, nil
	}
	run := s.startLocked(now)
	if run == nil {
		// Backing off after a failure: serve the previous value when there
		// is one (a stale credential surfaces as an upstream 401).
		value, err := s.value, s.lastErr
		s.mu.Unlock()
		if value != "" {
			return value, nil
		}
		return "", err
	}
	s.mu.Unlock()

	select {
	case <-run.done:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value != "" {
		return s.value, nil
	}
	return "", s.lastErr
}

// Invalidate implements DynamicSecretSource.
func (s *ExecSecretSource) Invalidate(rejected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rejected == "" || rejected != s.value || s.forced {
		return
	}
	now := s.now()
	if !s.lastForced.IsZero() && now.Sub(s.lastForced) < execInvalidateInterval {
		return
	}
	s.forced = true
	s.lastForced = now
}

// startLocked joins the in-flight run or starts one, returning nil while a
// failure backoff is in effect. The run uses its own timeout, never a
// caller's context: one caller giving up must not abort a refresh others
// are waiting on.
func (s *ExecSecretSource) startLocked(now time.Time) *execRun {
	if s.inflight != nil {
		return s.inflight
	}
	if now.Before(s.retryAt) {
		return nil
	}
	run := &execRun{done: make(chan struct{})}
	s.inflight = run
	go s.refresh(run)
	return run
}

func (s *ExecSecretSource) refresh(run *execRun) {
	started := s.now()
	value, err := s.execute()

	s.mu.Lock()
	s.inflight = nil
	s.forced = false
	if err != nil {
		s.failures++
		s.lastErr = err
		s.retryAt = s.now().Add(execBackoff(s.failures))
		if s.value != "" {
			log.Printf("upstream auth: %v; serving the previous credential until a refresh succeeds", err)
		}
	} else {
		ahead := min(s.ttl/5, execRefreshAheadCap)
		s.value = value
		s.expires = started.Add(s.ttl)
		s.refreshAt = s.expires.Add(-ahead)
		s.failures = 0
		s.lastErr = nil
		s.retryAt = time.Time{}
	}
	s.mu.Unlock()
	close(run.done)
}

// execBackoff doubles from execBackoffMin per consecutive failure, capped at
// execBackoffMax.
func execBackoff(failures int) time.Duration {
	d := execBackoffMin
	for i := 1; i < failures && d < execBackoffMax; i++ {
		d *= 2
	}
	return min(d, execBackoffMax)
}

// execute runs the command once and extracts the credential. Every error is
// a class description: output is never echoed, because it may be a partial
// or malformed credential.
func (s *ExecSecretSource) execute() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.argv[0], s.argv[1:]...)
	// nil Stdin and Stderr attach the null device; nil Env inherits.
	out := &cappedBuffer{limit: execOutputLimit}
	cmd.Stdout = out
	cmd.WaitDelay = execWaitDelay
	configureExecProcess(cmd)

	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("exec secret source: command timed out after %v", s.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			if code := exitErr.ExitCode(); code >= 0 {
				return "", fmt.Errorf("exec secret source: command exited with status %d", code)
			}
			return "", errors.New("exec secret source: command was terminated by a signal")
		case errors.Is(err, exec.ErrWaitDelay):
			// The command exited successfully but a descendant kept stdout
			// open past the wait delay; what it wrote before exiting is the
			// output.
		default:
			return "", fmt.Errorf("exec secret source: start command: %w", err)
		}
	}
	return firstLineCredential(out)
}

// firstLineCredential extracts the trimmed first line of the captured output.
func firstLineCredential(out *cappedBuffer) (string, error) {
	line, _, found := bytes.Cut(out.buf.Bytes(), []byte{'\n'})
	if !found && out.truncated {
		return "", fmt.Errorf("exec secret source: first output line exceeds %d bytes", execOutputLimit)
	}
	value := bytes.TrimSpace(line)
	if len(value) == 0 {
		return "", errors.New("exec secret source: command produced no output")
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return "", errors.New("exec secret source: command output contains control characters")
		}
	}
	return string(value), nil
}

// cappedBuffer retains at most limit bytes and silently discards the rest,
// always reporting a full write so the child never sees a broken pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}
