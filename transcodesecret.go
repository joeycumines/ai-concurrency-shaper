package main

import (
	"context"
	"os"
	"strings"
)

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
	data, err := os.ReadFile(string(s))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
