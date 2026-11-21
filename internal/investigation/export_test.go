package investigation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
)

func TestBundleMetadataRedactionRawAndExactDigest(t *testing.T) {
	c := diagnostic.New("epoch/1", 1024)
	c.Models("SECRET-alias", "SECRET-model", false)
	c.Versions(http.Header{"Anthropic-Beta": {"SECRET-header"}})
	c.SetBody(diagnostic.ClientRequest, []byte("SECRET-body"), "SECRET-type")
	c.Failure("request", "unknown_field", "$.SECRET-path", "", "SECRET-detail")
	u, _ := url.Parse("https://provider.invalid/SECRET-path?key=SECRET-query")
	s := Selection{Entries: []*journal.Entry{{ID: 1, RequestID: "epoch/1", Method: "POST", URL: u, RequestHeaders: http.Header{"Authorization": {"SECRET-token"}}, RequestBody: []byte("SECRET-body"), Diagnostic: c.Seal()}}, RequestIDs: []string{"epoch/1", "epoch/evicted"}}
	for _, raw := range []bool{false, true} {
		data, err := Encode(s, raw)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("SECRET")) != raw {
			t.Fatalf("raw=%v redaction incorrect: %s", raw, data)
		}
		var bundle Bundle
		if err = json.Unmarshal(data, &bundle); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(bundle.Entries)
		if hex.EncodeToString(hash[:]) != bundle.SHA256 || bundle.DigestScope != "entries_json_bytes" {
			t.Fatal("digest did not cover exact serialized entries")
		}
		if bundle.Count != 1 || len(bundle.MissingRequestIDs) != 1 {
			t.Fatal("evicted entry hidden")
		}
	}
	if c.Snapshot().Model != "SECRET-model" {
		t.Fatal("metadata export mutated local evidence")
	}
}
func TestExportFreshPrivateFilesAndFailureCleanup(t *testing.T) {
	dir := t.TempDir()
	first, err := WriteJSON(dir, "measurements", map[string]int{"requests": 3})
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteJSON(dir, "measurements", map[string]int{"requests": 4})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("overwrote earlier export")
	}
	data, err := os.ReadFile(first)
	if err != nil || !bytes.Contains(data, []byte("3")) {
		t.Fatal("original export changed")
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %v", info.Mode())
	}
	if _, err = WriteJSON(dir, "../escape", nil); err == nil {
		t.Fatal("accepted unsafe kind")
	}
	if _, err = WriteJSON(first, "logs", nil); err == nil {
		t.Fatal("accepted file as directory")
	}
	items, err := os.ReadDir(dir)
	if err != nil || len(items) != 2 {
		t.Fatal("failed export left partial files")
	}
}

func TestWriteJSONStreamingPreservesExactBytes(t *testing.T) {
	dir := t.TempDir()
	value := struct {
		Numbers []int             `json:"numbers"`
		Nested  map[string]string `json:"nested"`
	}{Numbers: []int{1, 2, 3}, Nested: map[string]string{"k": "v<&>"}}
	got, err := WriteJSON(dir, "measurements", value)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("streamed output differs from MarshalIndent:\n got: %s\nwant: %s", data, want)
	}
}

func TestWriteJSONRejectsOversizedValueWithoutOOM(t *testing.T) {
	// Each entry produces roughly 2 MiB of indented JSON, so the whole value
	// would exceed 64 MiB many times over. A bounded encoder must abort long
	// before the marshal buffer or the final file approaches that size.
	const (
		count = 80
		bytes = 1 << 21
	)
	entries := make([]map[string]any, count)
	for i := range entries {
		entries[i] = map[string]any{"body": strings.Repeat("x", bytes)}
	}
	dir := t.TempDir()
	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	if _, err := WriteJSON(dir, "measurements", entries); !errors.Is(err, ErrExportTooLarge) {
		t.Fatalf("oversized streaming export accepted: %v", err)
	}
	runtime.ReadMemStats(&memAfter)
	items, err := os.ReadDir(dir)
	if err != nil || len(items) != 0 {
		t.Fatalf("rejected export left files: %v %v", items, err)
	}
	// The encoder aborts as soon as the bounded sink reaches MaxExportBytes+1;
	// it never writes a full 160 MiB document or an oversized file.
	if grew := memAfter.HeapAlloc - memBefore.HeapAlloc; grew > 512<<20 {
		t.Fatalf("oversized export retained %d bytes of heap", grew)
	}
}

func TestRawExportBoundAndMetadataDoNotCopyLargeBodies(t *testing.T) {
	// Repeated immutable entries share their source body. The exporter must
	// reject their aggregate before cloning it into a much larger allocation.
	body := make([]byte, 8<<20)
	entry := &journal.Entry{RequestID: "epoch/large", RequestBody: body}
	selection := Selection{Entries: []*journal.Entry{entry, entry, entry, entry, entry, entry, entry}}
	if _, err := Encode(selection, true); !errors.Is(err, ErrExportTooLarge) {
		t.Fatalf("unbounded raw selection accepted: %v", err)
	}
	encoded, err := Encode(selection, false)
	if err != nil || len(encoded) > 10000 {
		t.Fatalf("metadata should remain small: %d %v", len(encoded), err)
	}
	var bundle Bundle
	if err = json.Unmarshal(encoded, &bundle); err != nil || !bundle.BodiesOmitted || bundle.Count != 7 {
		t.Fatalf("metadata omission not explicit: %+v %v", bundle, err)
	}
	dir := t.TempDir()
	if _, err = Write(dir, selection, true); !errors.Is(err, ErrExportTooLarge) {
		t.Fatal(err)
	}
	items, err := os.ReadDir(dir)
	if err != nil || len(items) != 0 {
		t.Fatal("rejected export created a file")
	}
}
