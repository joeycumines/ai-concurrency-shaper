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

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
)

func (p *Proxy) installSwitchingProtocolsProbeHooks(w http.ResponseWriter, attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64) {
	if p.breaker == nil {
		return
	}
	rec, ok := w.(*statusRecorder)
	if !ok {
		return
	}
	rec.onSwitchingProtocolsHandshakeSuccess = func() {
		attemptStart, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
		if attemptEpoch == 0 || !rec.resolveSwitchingProtocolsProbe() {
			return
		}
		p.breaker.RecordSuccess(attemptStart, attemptEpoch)
	}
	rec.onSwitchingProtocolsHandshakeFailure = func() {
		_, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
		if attemptEpoch == 0 || !rec.resolveSwitchingProtocolsProbe() {
			return
		}
		p.breaker.CancelProbe(attemptEpoch)
	}
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

var errLocalSwitchingProtocolsFailure = errors.New("proxy: local switching protocols failure")

// sanitizeTransportError rewrites a transport error before it is logged so no
// embedded target URL echoes credential query parameters: http.Transport
// wraps failures in url.Error with the full outbound URL. See
// auth.SanitizeURLError for the guarantees.
func sanitizeTransportError(err error) error {
	return auth.SanitizeURLError(err)
}

func localSwitchingProtocolsFailure(msg string) error {
	return fmt.Errorf("%w: %s", errLocalSwitchingProtocolsFailure, msg)
}

func validateSwitchingProtocolsResponse(res *http.Response) error {
	if res == nil || res.StatusCode != http.StatusSwitchingProtocols {
		return nil
	}
	if res.Request == nil {
		ensureSwitchingProtocolsResponseBodyCloseable(res)
		return localSwitchingProtocolsFailure("101 switching protocols response is missing request context")
	}
	reqUpType := switchingProtocolsUpgradeType(res.Request.Header)
	if reqUpType == "" {
		ensureSwitchingProtocolsResponseBodyCloseable(res)
		return fmt.Errorf("backend switched protocols without a requested Upgrade")
	}
	resUpType := switchingProtocolsUpgradeType(res.Header)
	if !isPrintableASCII(resUpType) {
		ensureSwitchingProtocolsResponseBodyCloseable(res)
		return fmt.Errorf("backend tried to switch to invalid protocol %q", resUpType)
	}
	if !strings.EqualFold(reqUpType, resUpType) {
		ensureSwitchingProtocolsResponseBodyCloseable(res)
		return fmt.Errorf("backend tried to switch protocol %q when %q was requested", resUpType, reqUpType)
	}
	if res.Body == nil {
		ensureSwitchingProtocolsResponseBodyCloseable(res)
		return localSwitchingProtocolsFailure("101 switching protocols response body is nil")
	}
	if _, ok := res.Body.(io.ReadWriteCloser); !ok {
		return localSwitchingProtocolsFailure("101 switching protocols response body must implement io.ReadWriteCloser")
	}
	if !downstreamCanHijackFromContext(res.Request.Context()) {
		return localSwitchingProtocolsFailure("downstream ResponseWriter cannot switch protocols")
	}
	rememberSwitchingProtocolsResponseBody(res.Request.Context(), res.Body)
	return nil
}

func ensureSwitchingProtocolsResponseBodyCloseable(res *http.Response) {
	if res != nil && res.Body == nil {
		res.Body = io.NopCloser(strings.NewReader(""))
	}
}

func switchingProtocolsUpgradeType(h http.Header) string {
	if !headerValuesContainToken(h["Connection"], "Upgrade") {
		return ""
	}
	return h.Get("Upgrade")
}

func headerValuesContainToken(values []string, token string) bool {
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

type switchingProtocolsResponseBodyStateKeyType struct{}

var switchingProtocolsResponseBodyStateKey = switchingProtocolsResponseBodyStateKeyType{}

type switchingProtocolsResponseBodyState struct {
	mu     sync.Mutex
	body   io.Closer
	closed bool
}

func withSwitchingProtocolsResponseBodyState(ctx context.Context) context.Context {
	return context.WithValue(ctx, switchingProtocolsResponseBodyStateKey, &switchingProtocolsResponseBodyState{})
}

func switchingProtocolsResponseBodyStateFromContext(ctx context.Context) *switchingProtocolsResponseBodyState {
	state, _ := ctx.Value(switchingProtocolsResponseBodyStateKey).(*switchingProtocolsResponseBodyState)
	return state
}

func rememberSwitchingProtocolsResponseBody(ctx context.Context, body io.Closer) {
	state := switchingProtocolsResponseBodyStateFromContext(ctx)
	if state == nil || body == nil {
		return
	}
	state.mu.Lock()
	state.body = body
	state.closed = false
	state.mu.Unlock()
}

func closeSwitchingProtocolsResponseBodyFromContext(ctx context.Context) {
	state := switchingProtocolsResponseBodyStateFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	body := state.body
	if state.closed {
		body = nil
	} else {
		state.closed = true
	}
	state.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
}

type downstreamUpgradeSupportKeyType struct{}

var downstreamUpgradeSupportKey = downstreamUpgradeSupportKeyType{}

func withDownstreamUpgradeSupport(ctx context.Context, canHijack bool) context.Context {
	return context.WithValue(ctx, downstreamUpgradeSupportKey, canHijack)
}

func downstreamCanHijackFromContext(ctx context.Context) bool {
	canHijack, _ := ctx.Value(downstreamUpgradeSupportKey).(bool)
	return canHijack
}

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func responseWriterCanHijack(w http.ResponseWriter) bool {
	for depth := 0; w != nil && depth < 32; depth++ {
		if rec, ok := w.(*statusRecorder); ok {
			w = rec.ResponseWriter
			continue
		}
		if _, ok := w.(http.Hijacker); ok {
			return true
		}
		uw, ok := w.(responseWriterUnwrapper)
		if !ok {
			return false
		}
		next := uw.Unwrap()
		if next == nil || next == w {
			return false
		}
		w = next
	}
	return false
}

func isLocalSwitchingProtocolsFailure(r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errLocalSwitchingProtocolsFailure) {
		return true
	}
	if r == nil || r.Header.Get("Upgrade") == "" {
		return false
	}
	// Go's ReverseProxy does not expose typed errors for local upgrade capability
	// failures. Pin the current strings with tests so a supported Go-version
	// behavior change is caught before it can poison upstream breaker health.
	msg := err.Error()
	return strings.Contains(msg, "can't switch protocols using non-Hijacker") ||
		strings.Contains(msg, "Hijack failed on protocol switch")
}

func isLocalSwitchingProtocolsNonHijackerFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "can't switch protocols using non-Hijacker")
}

type switchingProtocolsHandshakeWriter struct {
	writer    *bufio.Writer
	onSuccess func()
	tail      []byte
	once      sync.Once
}

func (w *switchingProtocolsHandshakeWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil {
		err = w.writer.Flush()
	}
	if err == nil && n == len(p) && n > 0 {
		combined := append(w.tail, p[:n]...)
		if bytes.Contains(combined, []byte("\r\n\r\n")) {
			w.once.Do(func() {
				if w.onSuccess != nil {
					w.onSuccess()
				}
			})
		}
		if len(combined) > 3 {
			w.tail = append(w.tail[:0], combined[len(combined)-3:]...)
		} else {
			w.tail = append(w.tail[:0], combined...)
		}
	}
	return n, err
}
