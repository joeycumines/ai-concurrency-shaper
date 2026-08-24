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

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretSource supplies an upstream secret on demand. Implementations must be
// safe for concurrent use; Secret may be called once per authenticated
// request.
type SecretSource interface {
	Secret(ctx context.Context) (string, error)
}

// SecretSourceFunc adapts a function to the SecretSource interface.
type SecretSourceFunc func(ctx context.Context) (string, error)

func (f SecretSourceFunc) Secret(ctx context.Context) (string, error) {
	return f(ctx)
}

// staticSecretSource holds a value resolved once at startup. Configurations
// resolve env/file references eagerly so a missing or empty credential fails
// before the listener binds instead of on the first request.
type staticSecretSource struct{ value string }

// NewStaticSecretSource wraps an already-resolved secret value.
func NewStaticSecretSource(value string) SecretSource {
	return staticSecretSource{value: value}
}

func (s staticSecretSource) Secret(context.Context) (string, error) {
	return s.value, nil
}

// envSecretSource reads a credential from an environment variable at call
// time. An unset variable and an empty value are both errors naming the
// variable: a missing credential must fail loudly, never silently inject "".
type envSecretSource struct{ variable string }

// NewEnvSecretSource returns a source reading the given environment variable.
func NewEnvSecretSource(variable string) SecretSource {
	return envSecretSource{variable: variable}
}

func (s envSecretSource) Secret(context.Context) (string, error) {
	value, ok := os.LookupEnv(s.variable)
	if !ok {
		return "", fmt.Errorf("environment variable %s is not set", s.variable)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("environment variable %s is empty", s.variable)
	}
	return value, nil
}

// fileSecretSource reads a credential from a file at call time. It exists for
// operator opt-in only: the documented custody model keeps provider secrets in
// the environment, never on disk.
type fileSecretSource struct{ path string }

// NewFileSecretSource returns a source reading the secret from path.
func NewFileSecretSource(path string) SecretSource {
	return fileSecretSource{path: path}
}

func (s fileSecretSource) Secret(context.Context) (string, error) {
	if s.path == "" {
		return "", errors.New("file secret source has no path")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("secret file %s is empty", s.path)
	}
	return value, nil
}
