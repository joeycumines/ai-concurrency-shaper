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

package journal

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJournal_BasicRecordAndEntries(t *testing.T) {
	j := New(4, 1024)

	if j.Len() != 0 {
		t.Fatalf("empty journal Len = %d, want 0", j.Len())
	}
	if j.Entries() != nil {
		t.Fatal("empty journal Entries should be nil")
	}

	e1 := &Entry{Method: "GET", URL: mustURL("/a")}
	e2 := &Entry{Method: "POST", URL: mustURL("/b")}
	j.Record(e1)
	j.Record(e2)

	if j.Len() != 2 {
		t.Fatalf("Len = %d, want 2", j.Len())
	}

	entries := j.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries len = %d, want 2", len(entries))
	}
	if entries[0].ID != 1 || entries[1].ID != 2 {
		t.Errorf("IDs = %d,%d, want 1,2", entries[0].ID, entries[1].ID)
	}
	if entries[0].Method != "GET" || entries[1].Method != "POST" {
		t.Errorf("Methods = %s,%s, want GET,POST", entries[0].Method, entries[1].Method)
	}
}

func TestJournal_Eviction(t *testing.T) {
	j := New(3, 1024)

	for i := 0; i < 5; i++ {
		j.Record(&Entry{Method: "GET", URL: mustURL("/" + string(rune('a'+i)))})
	}

	if j.Len() != 3 {
		t.Fatalf("Len = %d, want 3", j.Len())
	}

	entries := j.Entries()
	if len(entries) != 3 {
		t.Fatalf("Entries len = %d, want 3", len(entries))
	}

	// Oldest two (a, b) should be evicted. Remaining: c, d, e.
	expected := []string{"/c", "/d", "/e"}
	for i, e := range entries {
		if e.URL.Path != expected[i] {
			t.Errorf("entry[%d].URL.Path = %s, want %s", i, e.URL.Path, expected[i])
		}
	}

	// IDs should be 3, 4, 5.
	if entries[0].ID != 3 || entries[1].ID != 4 || entries[2].ID != 5 {
		t.Errorf("IDs = %d,%d,%d, want 3,4,5", entries[0].ID, entries[1].ID, entries[2].ID)
	}
}

func TestJournal_Clear(t *testing.T) {
	j := New(4, 1024)
	j.Record(&Entry{Method: "GET", URL: mustURL("/a")})
	j.Record(&Entry{Method: "POST", URL: mustURL("/b")})

	if j.Len() != 2 {
		t.Fatalf("before clear: Len = %d, want 2", j.Len())
	}

	j.Clear()

	if j.Len() != 0 {
		t.Fatalf("after clear: Len = %d, want 0", j.Len())
	}
	if j.Entries() != nil {
		t.Fatal("after clear: Entries should be nil")
	}

	// Sequence should reset.
	j.Record(&Entry{Method: "GET", URL: mustURL("/c")})
	entries := j.Entries()
	if entries[0].ID != 1 {
		t.Errorf("after clear+record: ID = %d, want 1", entries[0].ID)
	}
}

func TestJournal_Defaults(t *testing.T) {
	j := New(0, 0)
	if j.capacity != defaultCapacity {
		t.Errorf("capacity = %d, want %d", j.capacity, defaultCapacity)
	}
	if j.maxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("maxBodyBytes = %d, want %d", j.maxBodyBytes, defaultMaxBodyBytes)
	}
}

func TestJournal_Concurrent(t *testing.T) {
	j := New(256, 1024)
	const goroutines = 20
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				j.Record(&Entry{Method: "GET", URL: mustURL("/x")})
			}
		}(g)
	}
	wg.Wait()

	// All records should succeed; Len should be at capacity.
	if j.Len() != 256 {
		t.Fatalf("concurrent: Len = %d, want 256", j.Len())
	}

	// Total records = goroutines * perGoroutine = 2000.
	// IDs should go up to 2000.
	entries := j.Entries()
	if entries[len(entries)-1].ID != uint64(goroutines*perGoroutine) {
		t.Errorf("last ID = %d, want %d", entries[len(entries)-1].ID, goroutines*perGoroutine)
	}
}

func TestEntry_Name(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/messages", "messages"},
		{"/v1/chat/completions", "completions"},
		{"/api/v2/chat/completions", "completions"},
		{"/", "/"},
		{"", "/"}, // url.Parse("") gives empty path → "/" per Name() logic
	}
	for _, tt := range tests {
		e := &Entry{URL: mustURL(tt.path)}
		if got := e.Name(); got != tt.want {
			t.Errorf("Name(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}

	// Nil URL.
	e := &Entry{}
	if e.Name() != "" {
		t.Errorf("Name(nil URL) = %q, want empty", e.Name())
	}
}

func TestEntry_Type(t *testing.T) {
	tests := []struct {
		ct   string
		want string
	}{
		{"application/json", "json"},
		{"application/json; charset=utf-8", "json"},
		{"text/html", "html"},
		{"text/html; charset=utf-8", "html"},
		{"text/plain", "text"},
		{"text/event-stream", "events"},
		{"application/octet-stream", "binary"},
		{"application/x-www-form-urlencoded", "form"},
		{"multipart/form-data; boundary=abc", "form"},
		{"image/png", "png"},
		{"application/xml", "xml"},
		{"", "—"},
	}
	for _, tt := range tests {
		e := &Entry{ContentType: tt.ct}
		if got := e.Type(); got != tt.want {
			t.Errorf("Type(%q) = %q, want %q", tt.ct, got, tt.want)
		}
	}
}

func TestEntry_SizeLabel(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{-1, "—"},
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}
	for _, tt := range tests {
		e := &Entry{ResponseSize: tt.size}
		if got := e.SizeLabel(); got != tt.want {
			t.Errorf("SizeLabel(%d) = %q, want %q", tt.size, got, tt.want)
		}
	}
}

func TestTiming_Duration(t *testing.T) {
	now := time.Now()
	tt := Timing{
		QueueStart:       now,
		QueueEnd:         now.Add(100 * time.Millisecond),
		ResponseHeaders:  now.Add(300 * time.Millisecond),
		ResponseComplete: now.Add(500 * time.Millisecond),
	}
	if d := tt.Duration(); d != 500*time.Millisecond {
		t.Errorf("Duration = %v, want 500ms", d)
	}
	if d := tt.QueueDuration(); d != 100*time.Millisecond {
		t.Errorf("QueueDuration = %v, want 100ms", d)
	}
	if d := tt.TTFB(); d != 200*time.Millisecond {
		t.Errorf("TTFB = %v, want 200ms", d)
	}
	if d := tt.DownloadDuration(); d != 200*time.Millisecond {
		t.Errorf("DownloadDuration = %v, want 200ms", d)
	}
}

func TestTiming_ZeroValues(t *testing.T) {
	var tt Timing
	if d := tt.Duration(); d != 0 {
		t.Errorf("zero Duration = %v, want 0", d)
	}
	if d := tt.TTFB(); d != 0 {
		t.Errorf("zero TTFB = %v, want 0", d)
	}
}

func TestTeeReadCloser(t *testing.T) {
	content := "hello world"
	orig := io.NopCloser(strings.NewReader(content))

	rdr, cap := TeeReadCloser(orig, 1024)
	got, err := io.ReadAll(rdr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != content {
		t.Errorf("read = %q, want %q", string(got), content)
	}
	if string(cap.data) != content {
		t.Errorf("captured = %q, want %q", string(cap.data), content)
	}
	if cap.truncated {
		t.Error("should not be truncated")
	}
}

func TestTeeReadCloser_Truncated(t *testing.T) {
	content := "hello world, this is a long string"
	orig := io.NopCloser(strings.NewReader(content))

	rdr, cap := TeeReadCloser(orig, 5)
	got, err := io.ReadAll(rdr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != content {
		t.Errorf("read = %q, want %q", string(got), content)
	}
	if string(cap.data) != "hello" {
		t.Errorf("captured = %q, want %q", string(cap.data), "hello")
	}
	if !cap.truncated {
		t.Error("should be truncated")
	}
}

func TestTeeReadCloser_Close(t *testing.T) {
	orig := io.NopCloser(bytes.NewReader([]byte("data")))
	rdr, _ := TeeReadCloser(orig, 1024)
	if err := rdr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCaptureBuf_Write(t *testing.T) {
	cb := &CaptureBuf{maxBytes: 10}
	n, _ := cb.Write([]byte("hello"))
	if n != 5 {
		t.Errorf("n = %d, want 5", n)
	}
	n, _ = cb.Write([]byte("world!extra"))
	if n != 11 {
		t.Errorf("n = %d, want 11", n)
	}
	if !cb.truncated {
		t.Error("should be truncated")
	}
	if string(cb.data) != "helloworld" {
		t.Errorf("data = %q, want %q", string(cb.data), "helloworld")
	}

	// Further writes should be no-ops (bytes counted but not stored).
	n, _ = cb.Write([]byte("more"))
	if n != 4 {
		t.Errorf("post-truncation n = %d, want 4", n)
	}
	if len(cb.data) != 10 {
		t.Errorf("post-truncation len = %d, want 10", len(cb.data))
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Ensure types implement the interfaces we need.
var _ io.ReadCloser = (*CapturingReader)(nil)
var _ io.Writer = (*CaptureBuf)(nil)
