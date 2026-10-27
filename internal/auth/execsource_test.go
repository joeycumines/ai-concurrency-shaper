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
	"strings"
	"testing"
	"time"
)

func TestNewExecSecretSourceRejectsInvalidConfiguration(t *testing.T) {
	abs := "/usr/bin/true"
	cases := []struct {
		name    string
		argv    []string
		ttl     time.Duration
		timeout time.Duration
		want    string
	}{
		{name: "empty argv", argv: nil, ttl: time.Hour, timeout: time.Second, want: "command must not be empty"},
		{name: "empty command", argv: []string{""}, ttl: time.Hour, timeout: time.Second, want: "argument 0 is empty"},
		{name: "empty argument", argv: []string{abs, ""}, ttl: time.Hour, timeout: time.Second, want: "argument 1 is empty"},
		{name: "relative path", argv: []string{"./helper"}, ttl: time.Hour, timeout: time.Second, want: "absolute path or a name resolved through PATH"},
		{name: "relative nested path", argv: []string{"bin/helper"}, ttl: time.Hour, timeout: time.Second, want: "absolute path or a name resolved through PATH"},
		{name: "zero ttl", argv: []string{abs}, ttl: 0, timeout: time.Second, want: "ttl must be > 0"},
		{name: "negative ttl", argv: []string{abs}, ttl: -time.Second, timeout: time.Second, want: "ttl must be > 0"},
		{name: "zero timeout", argv: []string{abs}, ttl: time.Hour, timeout: 0, want: "timeout must be > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewExecSecretSource(tc.argv, tc.ttl, tc.timeout)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewExecSecretSource error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestNewExecSecretSourceAcceptsPathResolvedName(t *testing.T) {
	if _, err := NewExecSecretSource([]string{"credential-helper", "--flag"}, time.Hour, time.Second); err != nil {
		t.Fatalf("bare command name rejected: %v", err)
	}
}

func TestExecBackoffIsBounded(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := execBackoff(i + 1); got != w {
			t.Errorf("execBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := execBackoff(1000); got != execBackoffMax {
		t.Errorf("execBackoff(1000) = %v, want cap %v", got, execBackoffMax)
	}
}
