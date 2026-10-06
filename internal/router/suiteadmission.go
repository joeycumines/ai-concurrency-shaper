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

package router

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const (
	// catalogSuiteBufferBudgetBytes is the aggregate request-body budget the
	// admission pools mounted on one server divide between themselves.
	catalogSuiteBufferBudgetBytes = 64 << 20
	// catalogSuiteInspectionWait is the default body inspection timeout: how
	// long a request's body may take to arrive before the suite answers 408.
	// It is not an admission timeout — admission waits indefinitely, because a
	// request that has not been admitted is not buffering anything.
	catalogSuiteInspectionWait = 30 * time.Second
)

// CatalogSuiteAdmission bounds how many request bodies the catalog suites
// mounted on one server may be buffering at once. A slot is taken before a
// body is buffered and given back once the target that received the body
// spends it — read to exhaustion, closed, or handler return, whichever comes
// first.
//
// A request that finds the pool full WAITS for a slot. It is never turned away.
// What the wait costs is not the memory the pool bounds — a request is not
// read until it holds a slot, so a waiting request buffers nothing — but it
// is not free either: each waiter parks a goroutine and holds one idle
// connection whose request body was never read, and nothing here caps the
// number of them. Refusing instead would only hand the retry decision to the
// client, which is precisely what blocking request semantics are meant to
// avoid. The wait ends when a slot frees or the client disconnects.
//
// The release window is wider than the catalog's own work. It spans the
// target's admission wait as well, because a target that has not read the body
// yet cannot be relied on to release anything, and a target that only ever
// replays the body through r.GetBody never releases it at all. Releasing at
// consumption rather than at handler return is what keeps the pool from
// imposing a second, redundant ceiling on top of the provider limits the
// targets already enforce: once a target has spent the body, the request is no
// longer the catalog's to account for.
//
// The pool still bounds how many bodies are buffered while a saturated target
// makes its own callers wait, and that is the pool's actual job rather than a
// ceiling the providers impose twice. A request waiting for a provider slot
// has NOT spent its body — the bytes are still buffered here — so the catalog
// is still holding the memory the pool exists to bound. A later request waits
// behind it rather than displacing it.
//
// The pool does NOT bound process-wide request memory. Once a target has
// taken a body the bytes are its responsibility for the rest of the
// exchange: the catalog cannot know that no replay body (r.GetBody) will be
// requested before the handler returns, so it must keep them readable, and
// it holds no reference that could free them sooner. Aggregate buffered
// memory is therefore the budget plus whatever the downstream providers hold
// for their own in-flight exchanges.
type CatalogSuiteAdmission struct {
	slots chan struct{}
}

// NewCatalogSuiteAdmission sizes a pool so the suites sharing it can have at
// most catalogSuiteBufferBudgetBytes of request bodies buffered for targets
// that have not yet taken them, given each request's configured ceiling.
func NewCatalogSuiteAdmission(limits transcode.BodyLimits) *CatalogSuiteAdmission {
	limits = limits.WithDefaults()
	slots := max(1, int(catalogSuiteBufferBudgetBytes/max(int64(1), limits.AcceptedRequestBytes)))
	return &CatalogSuiteAdmission{slots: make(chan struct{}, slots)}
}

// acquireBufferSlot blocks until the pool has a slot for this request.
//
// There is deliberately no timeout and no "busy" error. A request that cannot
// be admitted yet is holding no buffered bytes — its body is not read until
// the slot is in hand — so waiting costs none of the memory the pool exists to
// bound. Rejecting instead would push the retry decision onto the client,
// which is exactly what blocking request semantics exist to avoid: the client
// call blocks until the proxy can admit it, so the client never needs its own
// backoff logic. The wait ends when a slot frees or the client goes away.
//
// The returned release is idempotent and safe to call from any goroutine, so
// the caller can hand ownership of it to a body that outlives this function.
func (h *CatalogSuiteHandler) acquireBufferSlot(r *http.Request) (func(), bool) {
	select {
	case h.admission.slots <- struct{}{}:
		var once sync.Once
		release := func() {
			once.Do(func() {
				<-h.admission.slots
			})
		}
		return release, true
	case <-r.Context().Done():
		return nil, false
	}
}

// catalogHandoff owns the buffered request body for the rest of the exchange
// and returns the admission token once that body is spent. The target is given
// one body on r.Body and fresh replay readers over the same bytes through
// r.GetBody; only the first is evidence that the catalog's buffer is gone, so
// only the first can give the token back.
type catalogHandoff struct {
	data        []byte
	releaseOnce sync.Once
	releaseSlot func()
}

func newCatalogHandoff(data []byte, releaseSlot func()) *catalogHandoff {
	return &catalogHandoff{
		data:        data,
		releaseSlot: releaseSlot,
	}
}

// drain returns the admission token. Every body, the handler-return backstop,
// and any of them concurrently may call it; exactly one call releases.
func (h *catalogHandoff) drain() {
	h.releaseOnce.Do(func() {
		if h.releaseSlot != nil {
			h.releaseSlot()
		}
	})
}

// primaryBody is the body installed on r.Body. Consuming or closing it is what
// gives the admission token back.
func (h *catalogHandoff) primaryBody() io.ReadCloser {
	return &catalogHandoffBody{reader: bytes.NewReader(h.data), onDrain: h.drain}
}

// replayBody is a fresh reader over the same bytes, for r.GetBody. It never
// releases: a target that drains a replay body has not drained the catalog's
// buffer, and treating that as release would let the pool admit more bodies
// than it exists to bound.
func (h *catalogHandoff) replayBody() io.ReadCloser {
	return &catalogHandoffBody{reader: bytes.NewReader(h.data)}
}

// catalogHandoffBody serves one read of the buffered request body.
//
// It implements io.WriterTo so the buffered body keeps the single-Write copy
// path a *bytes.Reader has always had here: before the handoff wrapper
// existed, the suite installed io.NopCloser over a *bytes.Reader, and any
// consumer that copies this body directly still gets one Write instead of a
// chunked copy through a 32 KiB buffer. A net/http Transport sending a body of
// known length does not take that path — transferWriter wraps a sized body in
// an io.LimitReader, which only reads — so this is about the direct
// consumers, not the sized send.
//
// mu serializes every operation that moves the reader offset, so a Read and a
// WriteTo on the same body cannot interleave and deliver or skip a byte twice.
// Close deliberately does NOT take mu: it only records that the body is spent,
// and a target is entitled to close from a goroutine other than the one
// copying, which must not block behind an in-flight write.
type catalogHandoffBody struct {
	mu      sync.Mutex
	reader  *bytes.Reader
	closed  atomic.Bool
	onDrain func()
}

func (b *catalogHandoffBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return 0, http.ErrBodyReadAfterClose
	}
	n, err := b.reader.Read(p)
	spent := b.spent()
	b.mu.Unlock()
	if spent {
		b.release()
	}
	return n, err
}

// WriteTo forwards the unread remainder in one call. It holds b.mu across the
// destination write, because the offset it advances belongs to the same
// critical section as Read's: releasing the lock around the write would let a
// concurrent Read move the offset and leave this advance resolving against a
// stale base. A short write leaves bytes behind, so the release still keys off
// exhaustion rather than off the return of this method.
//
// The unlock is deferred because the destination is caller code and may panic
// — bytes.Reader.WriteTo itself panics on a destination that reports more
// bytes than it was given. A bare Lock/Unlock pair would leave b.mu held for
// good, and because the target may recover that panic and go on to use the
// body, the handler would never return and the handler-return backstop would
// never release the admission token either. One wedged body would then cost
// the pool a slot for the life of the mount. A panicking write skips the
// release here, which is correct: the body is not spent, and the backstop
// returns the token when the handler unwinds.
func (b *catalogHandoffBody) WriteTo(w io.Writer) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	n, err := b.reader.WriteTo(w)
	if b.spent() {
		b.release()
	}
	return n, err
}

func (b *catalogHandoffBody) Close() error {
	b.closed.Store(true)
	b.release()
	return nil
}

// spent reports exhaustion and must be called with b.mu held. Exhaustion, not
// the error value, is the signal that the buffer is used up: a read that takes
// the last byte reports (n, nil), and bytes.Reader only reports io.EOF on a
// LATER read. A consumer that sizes its buffer from ContentLength, or uses
// io.ReadFull, never makes that later read — and never calls Close either.
func (b *catalogHandoffBody) spent() bool {
	return b.reader.Len() == 0
}

func (b *catalogHandoffBody) release() {
	if b.onDrain != nil {
		b.onDrain()
	}
}
