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

package config

import (
	"strings"
	"testing"
	"time"
)

// TestResolveAndValidate_ExecAuthSourceGrammar pins every rejected exec:
// configuration. All of them fail in the grammar pass, before any command
// could run.
func TestResolveAndValidate_ExecAuthSourceGrammar(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing ttl",
			args: []string{"-auth-source", `exec:["/usr/bin/true"]`},
			want: "-auth-source exec: requires -auth-source-ttl > 0, got 0s"},
		{name: "negative ttl",
			args: []string{"-auth-source", `exec:["/usr/bin/true"]`, "-auth-source-ttl", "-1m"},
			want: "requires -auth-source-ttl > 0"},
		{name: "zero timeout",
			args: []string{"-auth-source", `exec:["/usr/bin/true"]`, "-auth-source-ttl", "1h", "-auth-source-timeout", "0s"},
			want: "-auth-source-timeout must be > 0, got 0s"},
		{name: "bad json",
			args: []string{"-auth-source", `exec:/usr/bin/true`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a JSON array of strings"},
		{name: "non-string element",
			args: []string{"-auth-source", `exec:["/usr/bin/true",1]`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a JSON array of strings"},
		{name: "trailing data",
			args: []string{"-auth-source", `exec:["/usr/bin/true"] x`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a JSON array of strings"},
		{name: "empty argv",
			args: []string{"-auth-source", `exec:[]`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a non-empty JSON array of strings"},
		{name: "null argv",
			args: []string{"-auth-source", `exec:null`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a non-empty JSON array of strings"},
		{name: "no argv",
			args: []string{"-auth-source", `exec:`, "-auth-source-ttl", "1h"},
			want: "-auth-source exec: command must be a non-empty JSON array of strings"},
		{name: "empty argument",
			args: []string{"-auth-source", `exec:["/usr/bin/true",""]`, "-auth-source-ttl", "1h"},
			want: "argument 1 is empty"},
		{name: "relative command",
			args: []string{"-auth-source", `exec:["./helper"]`, "-auth-source-ttl", "1h"},
			want: "must be an absolute path or a name resolved through PATH"},
		{name: "ttl with env source",
			args: []string{"-auth-source", "env:X", "-auth-source-ttl", "1h"},
			want: "-auth-source-ttl requires an exec: -auth-source"},
		{name: "explicit zero ttl with file source",
			args: []string{"-auth-source", "file:/k", "-auth-source-ttl", "0s"},
			want: "-auth-source-ttl requires an exec: -auth-source"},
		{name: "ttl without source",
			args: []string{"-auth-source-ttl", "1h"},
			want: "-auth-source-ttl requires an exec: -auth-source"},
		{name: "timeout with env source",
			args: []string{"-auth-source", "env:X", "-auth-source-timeout", "60s"},
			want: "-auth-source-timeout requires an exec: -auth-source"},
		{name: "exec with mode none",
			args: []string{"-auth-source", `exec:["/usr/bin/true"]`, "-auth-source-ttl", "1h", "-auth-mode", "none"},
			want: "-auth-mode none does not use -auth-source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(append([]string{"-upstream", "https://x"}, tc.args...))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.ResolveAndValidate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveAndValidate error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestResolveAndValidate_ExecAuthSourceFlagsAreProviderScoped(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=a", "-upstream", "https://a.example",
		"-auth-source", "env:A", "-auth-source-timeout", "10s",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "-auth-source-timeout requires an exec: -auth-source") {
		t.Fatalf("ResolveAndValidate error = %v, want the timeout-without-exec rejection", err)
	}

	cfg, err = Parse([]string{"--provider=a", "-upstream", "https://a.example"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Providers[0].AuthSourceTimeout; got != 60*time.Second {
		t.Fatalf("default -auth-source-timeout = %v, want 60s", got)
	}
	if got := cfg.Providers[0].AuthSourceTTL; got != 0 {
		t.Fatalf("default -auth-source-ttl = %v, want 0 (unset)", got)
	}
}
