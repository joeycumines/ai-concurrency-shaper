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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
)

// newCommittedRecorder returns a recorder whose streaming representation was
// committed by the queue comments, with a journal entry to observe.
func newCommittedRecorder() (*statusRecorder, *httptest.ResponseRecorder) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{
		ResponseWriter:    inner,
		commentsCommitted: true,
		terminalWritten:   true,
		entry:             &journal.Entry{},
	}
	return rec, inner
}

// A superfluous failure status after the upstream's 2xx is ignored, as
// net/http ignores a second WriteHeader: the observed status, the journal
// entry, and the stream's close state all keep the first observation.
func TestStatusRecorderCommittedSuperfluousFailureKeepsUpstreamOK(t *testing.T) {
	rec, inner := newCommittedRecorder()
	rec.WriteHeader(http.StatusCreated)
	firstAt := rec.entry.Timing.ResponseHeaders
	time.Sleep(2 * time.Millisecond)

	rec.WriteHeader(http.StatusBadGateway)
	if got := rec.observedStatus(); got != http.StatusCreated {
		t.Fatalf("observed status = %d, want the upstream's %d", got, http.StatusCreated)
	}
	if rec.entry.StatusCode != http.StatusCreated {
		t.Fatalf("journal status = %d, want %d", rec.entry.StatusCode, http.StatusCreated)
	}
	if !rec.entry.Timing.ResponseHeaders.Equal(firstAt) {
		t.Fatalf("journal ResponseHeaders moved from %v to %v on a superfluous WriteHeader", firstAt, rec.entry.Timing.ResponseHeaders)
	}
	if strings.Contains(inner.Body.String(), "queue-wait failed") {
		t.Fatalf("superfluous failure wrote a close comment: %q", inner.Body.String())
	}
	if !rec.aborted {
		t.Fatal("aborted = false, want true: the handler still failed the stream")
	}
}

// Of two failure statuses with no 2xx observed, the first wins and the stream
// receives exactly one failed close.
func TestStatusRecorderCommittedFirstFailureWins(t *testing.T) {
	rec, inner := newCommittedRecorder()
	rec.WriteHeader(http.StatusInternalServerError)
	rec.WriteHeader(http.StatusBadGateway)
	if rec.upstreamStatusOverride != http.StatusInternalServerError {
		t.Fatalf("override = %d, want the first failure %d", rec.upstreamStatusOverride, http.StatusInternalServerError)
	}
	if rec.entry.StatusCode != http.StatusInternalServerError {
		t.Fatalf("journal status = %d, want %d", rec.entry.StatusCode, http.StatusInternalServerError)
	}
	if got := strings.Count(inner.Body.String(), "queue-wait failed: upstream"); got != 1 {
		t.Fatalf("failed-close comments = %d, want exactly 1 in %q", got, inner.Body.String())
	}
}
