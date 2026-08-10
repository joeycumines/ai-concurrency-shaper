package transcode

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestContractsLockDrift proves pins.md is generated from contracts.lock.json
// and the schema inventories are true to the lock's snapshot hashes: the
// checked-in pins.md must byte-match the regenerated document, and every
// inventory section must hash to the lock's snapshot_sha256. A hand-edited
// revision table or an inventory change without a deliberate pin bump fails
// here.
func TestContractsLockDrift(t *testing.T) {
	lock, err := LoadContractsLock()
	if err != nil {
		t.Fatal(err)
	}
	pins, err := os.ReadFile("pins.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyContractsLock(lock, pins); err != nil {
		t.Fatal(err)
	}
}

// TestContractsLockGenerationIdempotent proves regeneration is stable: the
// generated table contains exactly the lock's pins, and regenerating the
// regenerated document reproduces it byte-for-byte.
func TestContractsLockGenerationIdempotent(t *testing.T) {
	lock, err := LoadContractsLock()
	if err != nil {
		t.Fatal(err)
	}
	pins, err := os.ReadFile("pins.md")
	if err != nil {
		t.Fatal(err)
	}
	once, err := GeneratePinsMarkdown(lock, pins)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := GeneratePinsMarkdown(lock, once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatal("regeneration is not idempotent")
	}
	table := string(once)
	for _, want := range []string{
		"| OpenAI Responses | openai-go | v1.12.0 |",
		"| OpenAI Chat Completions | openai-go | v1.12.0 |",
		"| Anthropic Messages | anthropic-api | 2023-06-01 |",
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("generated table missing %q", want)
		}
	}
}

// TestContractsLockTamperDetection proves the drift test actually catches
// tampering: an edited revision table and a modified inventory both fail
// verification.
func TestContractsLockTamperDetection(t *testing.T) {
	lock, err := LoadContractsLock()
	if err != nil {
		t.Fatal(err)
	}
	pins, err := os.ReadFile("pins.md")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("edited table", func(t *testing.T) {
		tampered := bytes.Replace(
			pins,
			[]byte("| OpenAI Responses | openai-go | v1.12.0 |"),
			[]byte("| OpenAI Responses | openai-go | v9.9.9 |"),
			1,
		)
		if err := VerifyContractsLock(lock, tampered); err == nil {
			t.Fatal("tampered revision table verified clean")
		}
	})

	t.Run("edited inventory", func(t *testing.T) {
		tampered := bytes.Replace(
			pins,
			[]byte("id,required             string"),
			[]byte("id,required             integer"),
			1,
		)
		if err := VerifyContractsLock(lock, tampered); err == nil {
			t.Fatal("tampered inventory verified clean")
		}
	})
}

// TestContractsLockValidation proves malformed locks are rejected at load:
// missing source, missing version, and invalid hashes are construction
// errors.
func TestContractsLockValidation(t *testing.T) {
	valid := `{
		"openai_responses": {
			"source": "openai-go",
			"version": "v1.12.0",
			"snapshot_sha256": "` + strings.Repeat("ab", 32) + `"
		},
		"openai_chat_completions": {
			"source": "openai-go",
			"version": "v1.12.0",
			"snapshot_sha256": "` + strings.Repeat("cd", 32) + `"
		},
		"anthropic_messages": {
			"source": "anthropic-api",
			"version": "2023-06-01",
			"snapshot_sha256": "` + strings.Repeat("ef", 32) + `"
		}
	}`
	dir := t.TempDir()
	path := dir + "/lock.json"
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(valid)
	if _, err := LoadContractsLockFile(path); err != nil {
		t.Fatalf("valid lock rejected: %v", err)
	}

	cases := map[string]string{
		"missing source":  strings.Replace(valid, `"source": "openai-go",`, `"source": "",`, 1),
		"missing version": strings.Replace(valid, `"version": "v1.12.0",`, `"version": "",`, 1),
		"short hash":      strings.Replace(valid, strings.Repeat("ab", 32), "abcd", 1),
		"non-hex hash":    strings.Replace(valid, strings.Repeat("ab", 32), strings.Repeat("zz", 32), 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			write(doc)
			if _, err := LoadContractsLockFile(path); err == nil {
				t.Fatal("invalid lock accepted")
			}
		})
	}
}
