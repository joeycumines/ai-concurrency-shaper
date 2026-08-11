package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxSecretFileBytes bounds a secret file read at 64 KiB with one-byte
// overflow detection: an oversized file is a construction error, never an
// unbounded read (review-z commit 4). The file is re-read per request so
// atomic file replacement rotates credentials without a restart.
const maxSecretFileBytes = 64 << 10

// envSecretSource reads a secret from an environment variable.
type envSecretSource string

func (s envSecretSource) Secret(context.Context) (string, error) {
	value := os.Getenv(string(s))
	return strings.TrimSpace(value), nil
}

// fileSecretSource reads a secret from a file path. The file's trailing
// newline is trimmed.
type fileSecretSource string

func (s fileSecretSource) Secret(_ context.Context) (string, error) {
	file, err := os.Open(string(s))
	if err != nil {
		return "", err
	}
	defer file.Close()
	// Bounded read: 64 KiB cap plus one-byte overflow detection. The file
	// is re-read on every request, so atomic file replacement rotates the
	// credential without a restart.
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf(
			"secret file %q exceeds the %d byte bound",
			string(s),
			maxSecretFileBytes,
		)
	}
	return strings.TrimSpace(string(data)), nil
}
