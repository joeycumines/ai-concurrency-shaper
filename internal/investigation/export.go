// Package investigation writes operator-selected, versioned exports. Selection
// is frozen before IO; files are never overwritten and partial files are removed
// only when this writer created them.
package investigation

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
)

type Selection struct {
	Entries    []*journal.Entry
	RequestIDs []string
	Label      string
}

// MaxExportBytes bounds an individual export, including its envelope. An
// operator can narrow the selection and export another independent bundle.
const MaxExportBytes = 64 << 20

var ErrExportTooLarge = errors.New("export exceeds 64 MiB; narrow the selection")

type Bundle struct {
	Schema            string    `json:"schema"`
	Created           time.Time `json:"created"`
	Raw               bool      `json:"raw"`
	BodiesOmitted     bool      `json:"bodies_omitted"`
	Build             string    `json:"build"`
	Count             int       `json:"count"`
	MissingRequestIDs []string  `json:"missing_request_ids,omitempty"`
	DigestScope       string    `json:"digest_scope"`
	SHA256            string    `json:"sha256"`
	// Entries is exactly the byte array covered by SHA256, not a claim that
	// the whole JSON file or its mutable envelope is signed.
	Entries json.RawMessage `json:"entries"`
}
type entry struct {
	RequestID         string               `json:"request_id"`
	JournalID         uint64               `json:"journal_id"`
	Method            string               `json:"method"`
	Status            int                  `json:"status"`
	Timing            journal.Timing       `json:"timing"`
	Limited           bool                 `json:"limited"`
	Aborted           bool                 `json:"aborted"`
	Attempt           int                  `json:"attempt"`
	Evidence          *diagnostic.Snapshot `json:"evidence,omitempty"`
	URL               string               `json:"url,omitempty"`
	RequestHeaders    http.Header          `json:"request_headers,omitempty"`
	ResponseHeaders   http.Header          `json:"response_headers,omitempty"`
	RequestBody       []byte               `json:"request_body,omitempty"`
	ResponseBody      []byte               `json:"response_body,omitempty"`
	RequestTruncated  bool                 `json:"request_truncated"`
	ResponseSize      int64                `json:"response_size"`
	ResponseTruncated bool                 `json:"response_truncated"`
}

// Encode returns a stable snapshot of the selected entries. Metadata mode
// excludes arbitrary HTTP data, free error text, models, SDK header values and
// paths; safe failure taxonomy and contract fingerprints remain useful.
func Encode(s Selection, raw bool) ([]byte, error) {
	if len(s.Entries) > 10000 || len(s.RequestIDs) > 10000 {
		return nil, errors.New("investigation selection exceeds 10000 entries")
	}
	// Reject oversized raw selections before copying any body. Byte slices
	// become base64 in JSON; arbitrary HTTP strings may need six-byte escapes.
	if raw {
		var size int64
		for _, v := range s.Entries {
			if v != nil {
				size += rawSizeBound(v)
				if size > MaxExportBytes {
					return nil, ErrExportTooLarge
				}
			}
		}
	}
	var entries bytes.Buffer
	entries.WriteByte('[')
	count := 0
	found := make(map[string]bool, len(s.Entries))
	for _, v := range s.Entries {
		if v == nil {
			continue
		}
		found[v.RequestID] = true
		e := entry{RequestID: v.RequestID, JournalID: v.ID, Method: safeMethod(v.Method), Status: v.StatusCode, Timing: v.Timing, Limited: v.Limited, Aborted: v.Aborted, Attempt: v.Attempt, RequestTruncated: v.RequestBodyTruncated, ResponseSize: v.ResponseSize, ResponseTruncated: v.ResponseSize > int64(len(v.ResponseBody))}
		if raw {
			e.Evidence = diagnostic.Clone(v.Diagnostic)
			if v.URL != nil {
				e.URL = v.URL.String()
			}
			e.RequestHeaders = v.RequestHeaders.Clone()
			e.ResponseHeaders = v.ResponseHeaders.Clone()
			e.RequestBody = append([]byte(nil), v.RequestBody...)
			e.ResponseBody = append([]byte(nil), v.ResponseBody...)
		} else {
			e.Evidence = diagnostic.CloneMetadata(v.Diagnostic)
			redactEvidence(e.Evidence)
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if len(encoded) > MaxExportBytes-entries.Len()-2 {
			return nil, ErrExportTooLarge
		}
		if count != 0 {
			entries.WriteByte(',')
		}
		entries.Write(encoded)
		count++
	}
	entries.WriteByte(']')
	data := entries.Bytes()
	digest := sha256.Sum256(data)
	build := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		build = info.GoVersion + " " + info.Main.Path + "@" + info.Main.Version
	}
	b := Bundle{Schema: "shaper.investigation/v1", Created: time.Now().UTC(), Raw: raw, BodiesOmitted: !raw, Build: build, Count: count, DigestScope: "entries_json_bytes", SHA256: hex.EncodeToString(digest[:]), Entries: data}
	for _, id := range s.RequestIDs {
		if !found[id] {
			b.MissingRequestIDs = append(b.MissingRequestIDs, id)
		}
	}
	// Compact encoding is deliberate: re-indenting a RawMessage would change
	// the exact entries bytes under the digest. Readers may format for display.
	encoded, err := json.Marshal(b)
	if len(encoded) > MaxExportBytes {
		return nil, ErrExportTooLarge
	}
	return encoded, err
}

func rawSizeBound(v *journal.Entry) int64 {
	encodedBytes := func(n int) int64 { return (int64(n) + 2) / 3 * 4 }
	size := encodedBytes(len(v.RequestBody)) + encodedBytes(len(v.ResponseBody))
	if v.Diagnostic != nil {
		for _, leg := range v.Diagnostic.Legs {
			size += encodedBytes(len(leg.Body))
		}
	}
	if v.URL != nil {
		size += 6 * int64(len(v.URL.String()))
	}
	for _, h := range []http.Header{v.RequestHeaders, v.ResponseHeaders} {
		for key, values := range h {
			size += 6*int64(len(key)) + 8
			for _, value := range values {
				size += 6*int64(len(value)) + 4
			}
		}
	}
	return size
}
func safeMethod(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return m
	}
	return "OTHER"
}
func redactEvidence(s *diagnostic.Snapshot) {
	if s == nil {
		return
	}
	s.Model = ""
	s.ClientModel = ""
	s.ClientVersions = nil
	for i := range s.Legs {
		s.Legs[i].Body = nil
		switch s.Legs[i].ContentType {
		case "application/json", "text/event-stream", "text/plain":
		default:
			s.Legs[i].ContentType = ""
		}
	}
	for i := range s.Decisions {
		s.Decisions[i].Path = ""
		s.Decisions[i].Detail = ""
	}
	for i := range s.Failures {
		s.Failures[i].Path = ""
		s.Failures[i].Detail = ""
	}
}

func Write(dir string, s Selection, raw bool) (string, error) {
	data, err := Encode(s, raw)
	if err != nil {
		return "", err
	}
	kind := "investigation"
	if raw {
		kind += "-RAW"
	}
	return writeBytes(dir, kind, data)
}

// boundedBuffer accepts at most limit+1 bytes. Reaching the limit aborts the
// encoder through io.ErrShortWrite, so an oversized export never accumulates a
// second, multi-gigabyte copy in memory.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit + 1 - len(b.buf)
	if remaining <= 0 {
		return 0, io.ErrShortWrite
	}
	n := min(len(p), remaining)
	if cap(b.buf)-len(b.buf) < n {
		// Grow with headroom but never past the bound: appends must not
		// temporarily allocate a second multi-gigabyte backing array.
		capacity := min(max(2*cap(b.buf), 4096, len(b.buf)+n), b.limit+1)
		next := make([]byte, len(b.buf), capacity)
		copy(next, b.buf)
		b.buf = next
	}
	b.buf = append(b.buf, p[:n]...)
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

// v1IndentOptions reproduces json.MarshalIndent's syntax (HTML and U+2028/2029
// escaping, invalid UTF-8 replacement, raw-string preservation) without its
// accumulate-everything behavior. jsonv2's streaming encoder writes through to
// the bounded sink instead, so an oversized value aborts early.
var v1IndentOptions = []jsontext.Options{
	jsontext.WithIndent("  "),
	jsontext.EscapeForHTML(true),
	jsontext.EscapeForJS(true),
	jsontext.AllowInvalidUTF8(true),
	jsontext.PreserveRawStrings(true),
}

// WriteJSON exports immutable measurements, selected request facts, logs or
// control audit data. This generic function does not redact its caller's data;
// the terminal names that scope explicitly before an export.
func WriteJSON(dir, kind string, value any) (string, error) {
	var sink boundedBuffer
	sink.limit = MaxExportBytes
	err := jsonv2.MarshalWrite(&sink, value, v1IndentOptions...)
	if err != nil {
		if errors.Is(err, io.ErrShortWrite) {
			return "", ErrExportTooLarge
		}
		return "", err
	}
	data := sink.buf
	if len(data) > MaxExportBytes {
		return "", ErrExportTooLarge
	}
	return writeBytes(dir, kind, data)
}
func writeBytes(dir, kind string, data []byte) (string, error) {
	if dir == "" {
		dir = "."
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("export destination is not a directory")
	}
	for _, r := range kind {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return "", errors.New("invalid export kind")
		}
	}
	if kind == "" || len(kind) > 48 {
		return "", errors.New("invalid export kind")
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("shaper-%s-%s-%s.json", kind, time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(suffix[:])))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err = io.Copy(f, bytes.NewReader(data)); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}
