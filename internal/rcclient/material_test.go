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

package rcclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeMaterialEmpty(t *testing.T) {
	b, err := DecodeMaterial("")
	if err != nil {
		t.Fatalf("empty: unexpected error: %v", err)
	}
	if b != nil {
		t.Fatalf("empty: want nil, got %q", b)
	}
}

func TestDecodeMaterialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	want := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := DecodeMaterial("file:" + path)
	if err != nil {
		t.Fatalf("file: unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("file: got %q want %q", got, want)
	}
}

func TestDecodeMaterialFileEmptyPath(t *testing.T) {
	if _, err := DecodeMaterial("file:"); err == nil {
		t.Fatal("file: with empty path: want error, got nil")
	}
}

func TestDecodeMaterialFileMissing(t *testing.T) {
	if _, err := DecodeMaterial("file:/nonexistent/rcclient-material-test.pem"); err == nil {
		t.Fatal("file: missing file: want error, got nil")
	}
}

func TestDecodeMaterialBase64(t *testing.T) {
	// base64("hello") == "aGVsbG8="
	got, err := DecodeMaterial("base64:aGVsbG8=")
	if err != nil {
		t.Fatalf("base64: unexpected error: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("base64: got %q want %q", got, "hello")
	}
}

func TestDecodeMaterialBase64Invalid(t *testing.T) {
	if _, err := DecodeMaterial("base64:!!!not-base64!!!"); err == nil {
		t.Fatal("base64: invalid data: want error, got nil")
	}
}

func TestDecodeMaterialNoPrefix(t *testing.T) {
	if _, err := DecodeMaterial("raw-material-without-prefix"); err == nil {
		t.Fatal("no prefix: want error, got nil")
	}
}

// TestDecodeMaterialNoPrefixDoesNotLeak pins the secret-hygiene contract:
// the error for unprefixed material must never echo the value back (an
// operator who pastes a raw private key without a prefix would otherwise
// leak it into the journal/log). Only the size may be reported.
func TestDecodeMaterialNoPrefixDoesNotLeak(t *testing.T) {
	secret := "-----BEGIN PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END PRIVATE KEY-----"
	_, err := DecodeMaterial(secret)
	if err == nil {
		t.Fatal("no prefix: want error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the raw material: %v", err)
	}
	if !strings.Contains(err.Error(), "file: or base64: prefix") {
		t.Fatalf("error = %v, want it to name the required prefixes", err)
	}
}
