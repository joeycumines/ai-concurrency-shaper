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

// Package journal provides a thread-safe ring buffer of HTTP request/response
// pairs for the TUI's Network inspection panel.
//
// A Journal is safe for concurrent use from many goroutines. It stores
// immutable Entry values in a fixed-size ring; when full, the oldest entry
// is evicted to make room for the new one.
package journal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// defaultCapacity is the maximum number of entries retained.
	defaultCapacity = 512
	// defaultMaxBodyBytes is the maximum request/response body size captured
	// per entry. Bodies larger than this are truncated.
	defaultMaxBodyBytes = 1 << 20 // 1 MiB
)

// Timing holds the phased timing breakdown for a single request/response
// exchange, mirroring the phases shown in Chrome DevTools' Network panel.
type Timing struct {
	QueueStart       time.Time
	QueueEnd         time.Time
	ResponseHeaders  time.Time
	ResponseComplete time.Time
}

// Duration returns the total wall-clock time from the earliest non-zero
// timestamp to the latest.
func (t Timing) Duration() time.Duration {
	start := t.QueueStart
	if start.IsZero() {
		start = t.QueueEnd
	}
	end := t.ResponseComplete
	if end.IsZero() {
		end = t.ResponseHeaders
	}
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start)
}

// TTFB returns time-to-first-byte: from proxy start to first response byte.
func (t Timing) TTFB() time.Duration {
	if t.QueueEnd.IsZero() || t.ResponseHeaders.IsZero() {
		return 0
	}
	return t.ResponseHeaders.Sub(t.QueueEnd)
}

// QueueDuration returns the time spent waiting in the concurrency queue.
func (t Timing) QueueDuration() time.Duration {
	if t.QueueStart.IsZero() || t.QueueEnd.IsZero() {
		return 0
	}
	return t.QueueEnd.Sub(t.QueueStart)
}

// DownloadDuration returns the time from first response byte to body complete.
func (t Timing) DownloadDuration() time.Duration {
	if t.ResponseHeaders.IsZero() || t.ResponseComplete.IsZero() {
		return 0
	}
	return t.ResponseComplete.Sub(t.ResponseHeaders)
}

// Entry is an immutable record of one HTTP request/response exchange.
type Entry struct {
	ID             uint64
	Method         string
	URL            *url.URL
	RequestHeaders http.Header
	RequestBody    []byte
	// RequestBodyTruncated reports that RequestBody is not the complete
	// request body: the capture hit its byte limit, or the transport stopped
	// reading before EOF (an upstream that answered before consuming it).
	RequestBodyTruncated bool
	StatusCode           int
	ResponseHeaders      http.Header
	ResponseBody         []byte
	Timing               Timing
	Limited              bool
	Aborted              bool
	Attempt              int
	ContentType          string
	ResponseSize         int64
}

// Name returns the last meaningful segment of the URL path.
func (e *Entry) Name() string {
	if e.URL == nil {
		return ""
	}
	path := strings.TrimRight(e.URL.Path, "/")
	if path == "" {
		return "/"
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// Type returns a short content-type label for the response.
func (e *Entry) Type() string {
	ct := e.ContentType
	if ct == "" {
		return "—"
	}
	if semi := strings.Index(ct, ";"); semi >= 0 {
		ct = strings.TrimSpace(ct[:semi])
	}
	switch ct {
	case "application/json":
		return "json"
	case "text/html":
		return "html"
	case "text/plain":
		return "text"
	case "text/event-stream":
		return "events"
	case "application/octet-stream":
		return "binary"
	case "application/x-www-form-urlencoded":
		return "form"
	case "multipart/form-data":
		return "form"
	}
	if slash := strings.LastIndex(ct, "/"); slash >= 0 {
		return ct[slash+1:]
	}
	return ct
}

// SizeLabel returns a human-readable response size.
func (e *Entry) SizeLabel() string {
	n := e.ResponseSize
	if n < 0 {
		return "—"
	}
	if n == 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= div && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div/unit), "KMGT"[exp-1])
}

// Journal is a thread-safe ring buffer of Entry values.
type Journal struct {
	mu       sync.RWMutex
	entries  []*Entry
	head     int
	count    int
	capacity int
	seq      uint64

	maxBodyBytes int64
}

// New creates a Journal with the given capacity and per-entry body size limit.
func New(capacity int, maxBodyBytes int64) *Journal {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	return &Journal{
		entries:      make([]*Entry, capacity),
		capacity:     capacity,
		maxBodyBytes: maxBodyBytes,
	}
}

// MaxBodyBytes returns the per-entry body size limit.
func (j *Journal) MaxBodyBytes() int64 {
	return j.maxBodyBytes
}

// Record adds an entry to the journal. The entry's ID is set by Record.
func (j *Journal) Record(e *Entry) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	e.ID = j.seq

	if j.count < j.capacity {
		idx := (j.head + j.count) % j.capacity
		j.entries[idx] = e
		j.count++
	} else {
		j.entries[j.head] = e
		j.head = (j.head + 1) % j.capacity
	}
}

// Entries returns a snapshot of all entries in chronological order.
func (j *Journal) Entries() []*Entry {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if j.count == 0 {
		return nil
	}

	out := make([]*Entry, j.count)
	for i := 0; i < j.count; i++ {
		out[i] = j.entries[(j.head+i)%j.capacity]
	}
	return out
}

// Len returns the current number of entries.
func (j *Journal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.count
}

// Clear removes all entries.
func (j *Journal) Clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.head = 0
	j.count = 0
	j.seq = 0
	for i := range j.entries {
		j.entries[i] = nil
	}
}

// CapturingReader wraps a ReadCloser to tee bytes into a buffer as they
// are read. It implements io.ReadCloser.
type CapturingReader struct {
	io.Reader
	orig    io.ReadCloser
	capture *CaptureBuf
}

// Read tees the source bytes and records EOF so the capture can report
// whether the body was fully read.
func (c *CapturingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		c.capture.markEOF()
	}
	return n, err
}

func (c *CapturingReader) Close() error {
	return c.orig.Close()
}

// CaptureBuf is an io.Writer that accumulates bytes up to a limit. It is safe
// for concurrent use: the tee writes from the transport's write loop, which by
// the RoundTripper contract can outlive RoundTrip, while the proxy reads the
// capture after the exchange.
type CaptureBuf struct {
	mu        sync.Mutex
	data      []byte
	maxBytes  int64
	truncated bool
	eof       bool
}

func (cb *CaptureBuf) Write(p []byte) (int, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.truncated {
		return len(p), nil
	}
	remaining := cb.maxBytes - int64(len(cb.data))
	if remaining <= 0 {
		cb.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		cb.data = append(cb.data, p[:remaining]...)
		cb.truncated = true
		return len(p), nil
	}
	cb.data = append(cb.data, p...)
	return len(p), nil
}

// Bytes returns a copy of the captured bytes (may be truncated or partial;
// see Truncated and Complete).
func (cb *CaptureBuf) Bytes() []byte {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return bytes.Clone(cb.data)
}

// Snapshot returns a copy of the captured bytes together with the truncation
// and completeness flags, read under one lock so the three facts describe the
// same instant. Reading them through separate calls would let the writer
// append the final bytes and mark EOF in between, recording a strict prefix as
// a complete body.
func (cb *CaptureBuf) Snapshot() (data []byte, truncated, complete bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return bytes.Clone(cb.data), cb.truncated, cb.eof
}

// Truncated reports whether the capture hit its byte limit.
func (cb *CaptureBuf) Truncated() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.truncated
}

// Complete reports whether the source was read to EOF. A false result means
// the captured bytes are a prefix of the request body: the transport stopped
// reading early (an upstream that answered before consuming it) or the source
// never returned EOF.
func (cb *CaptureBuf) Complete() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.eof
}

// markEOF records that the source returned EOF.
func (cb *CaptureBuf) markEOF() {
	cb.mu.Lock()
	cb.eof = true
	cb.mu.Unlock()
}

// TeeReadCloser wraps r so that reads are teed into a capture buffer. The
// capture is partial when the reader stops before EOF (an upstream that
// answers early) or the byte limit is hit; take one CaptureBuf.Snapshot to
// read the bytes and both flags together.
func TeeReadCloser(r io.ReadCloser, maxBytes int64) (*CapturingReader, *CaptureBuf) {
	cb := &CaptureBuf{maxBytes: maxBytes}
	return &CapturingReader{
		Reader:  io.TeeReader(r, cb),
		orig:    r,
		capture: cb,
	}, cb
}
