package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileSecretSourceBounded proves the secret file read is bounded at
// maxSecretFileBytes with one-byte overflow detection, and the file is
// re-read per request so atomic replacement rotates the credential.
func TestFileSecretSourceBounded(t *testing.T) {
	dir := t.TempDir()

	t.Run("oversized file rejected", func(t *testing.T) {
		path := filepath.Join(dir, "too-large")
		if err := os.WriteFile(path, []byte(strings.Repeat("k", maxSecretFileBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		if _, err := source.Secret(context.Background()); err == nil {
			t.Fatal("oversized secret file accepted")
		} else if !strings.Contains(err.Error(), "byte bound") {
			t.Fatalf("err = %v, want the bound error", err)
		}
	})

	t.Run("at bound accepted and trimmed", func(t *testing.T) {
		path := filepath.Join(dir, "at-bound")
		// Exactly the bound, no trailing whitespace to trim.
		content := strings.Repeat("k", maxSecretFileBytes)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		secret, err := source.Secret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != maxSecretFileBytes {
			t.Fatalf("secret length = %d, want %d", len(secret), maxSecretFileBytes)
		}
	})

	t.Run("rotation without restart", func(t *testing.T) {
		path := filepath.Join(dir, "rotating")
		if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		if secret, _ := source.Secret(context.Background()); secret != "first" {
			t.Fatalf("secret = %q", secret)
		}
		// Atomic replacement (rename) rotates the credential.
		replacement := filepath.Join(dir, "replacement")
		if err := os.WriteFile(replacement, []byte("second\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		if secret, _ := source.Secret(context.Background()); secret != "second" {
			t.Fatalf("rotated secret = %q", secret)
		}
	})
}
