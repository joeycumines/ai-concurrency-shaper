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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// extractModelFromBody is the Task 12 prototype helper that reads up to maxBytes
// from r.Body, extracts the model string, and restores r.Body for downstream consumers.
func extractModelFromBody(r *http.Request, maxBytes int64) (string, error) {
	if r.Body == nil || r.Method != http.MethodPost {
		return "", nil
	}

	// Read bounded bytes + 1 to detect overflow
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(buf)) > maxBytes {
		return "", errors.New("request body exceeds maximum model routing buffer")
	}

	// Restore r.Body so downstream consumers (retry, journal, reverse proxy) can read it
	r.Body = io.NopCloser(bytes.NewReader(buf))

	// Fast JSON model extraction
	var payload struct {
		Model string `json:"model"`
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := dec.Decode(&payload); err != nil {
		return "", errors.New("malformed JSON payload in model routing")
	}
	return payload.Model, nil
}

// TestPrototype_BodyTee_ModelExtractionAndReplaySafety proves the Task 12 spike's
// body-tee mechanism:
//  1. Correctly extracts the model identifier from JSON bodies.
//  2. Fails closed with an error on malformed JSON or bodies exceeding bounds.
//  3. Restores r.Body so that subsequent downstream reads (simulating proxy forward,
//     retry replay, and journal preview) receive the exact payload byte-for-byte.
func TestPrototype_BodyTee_ModelExtractionAndReplaySafety(t *testing.T) {
	const maxBytes = 1024 // 1 KiB for test

	// Case 1: Valid payload with model field
	payload := `{"model":"claude-3.5-sonnet","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	model, err := extractModelFromBody(req, maxBytes)
	if err != nil {
		t.Fatalf("unexpected extract error: %v", err)
	}
	if model != "claude-3.5-sonnet" {
		t.Errorf("model = %q, want %q", model, "claude-3.5-sonnet")
	}

	// Downstream read 1: Simulating retry transport replay buffer copy
	retryBuf := new(bytes.Buffer)
	if _, err := io.Copy(retryBuf, req.Body); err != nil {
		t.Fatalf("retry buffer copy failed: %v", err)
	}
	if retryBuf.String() != payload {
		t.Fatalf("retry buffer got %q, want %q", retryBuf.String(), payload)
	}

	// Downstream read 2: Simulating journal request body capture from restored buffer
	req.Body = io.NopCloser(bytes.NewReader(retryBuf.Bytes()))
	journalBuf, err := io.ReadAll(io.LimitReader(req.Body, 512))
	if err != nil {
		t.Fatalf("journal capture failed: %v", err)
	}
	if string(journalBuf) != payload {
		t.Fatalf("journal capture got %q, want %q", string(journalBuf), payload)
	}

	// Case 2: Body exceeds maxBytes -> fast fail
	largePayload := `{"model":"gpt-4o","data":"` + strings.Repeat("x", 2000) + `"}`
	reqLarge := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(largePayload))
	if _, err := extractModelFromBody(reqLarge, maxBytes); err == nil {
		t.Fatal("expected error on oversized body, got nil")
	}

	// Case 3: Malformed JSON -> fast fail
	malformed := `{"model":"claude", "messages": [unterminated`
	reqMalformed := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(malformed))
	if _, err := extractModelFromBody(reqMalformed, maxBytes); err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}
