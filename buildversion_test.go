package main

import "testing"

func TestModuleReleaseVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "release", in: "v0.9.2", want: "v0.9.2"},
		{name: "devel", in: "(devel)", want: "dev"},
		{name: "empty", in: "", want: "dev"},
		{name: "zero pseudo version", in: "v0.0.0-20261126042522-46906dc3d97e", want: "dev"},
		{name: "post tag pseudo version", in: "v0.9.3-0.20261126042522-46906dc3d97e", want: "dev"},
		{name: "post tag pseudo version with dirty metadata", in: "v0.9.3-0.20261126042522-46906dc3d97e+dirty", want: "dev"},
		{name: "prerelease pseudo version", in: "v1.2.3-pre.0.20261126042522-46906dc3d97e", want: "dev"},
		{name: "base pseudo version", in: "v1.0.0-20261126042522-46906dc3d97e", want: "dev"},

		{name: "prerelease tag", in: "v0.9.3-rc.1", want: "v0.9.3-rc.1"},
		{name: "zero prerelease tag", in: "v0.0.0-alpha", want: "v0.0.0-alpha"},
		{name: "incompatible release tag", in: "v2.0.0+incompatible", want: "v2.0.0+incompatible"},
		{name: "date stamped prerelease tag", in: "v1.2.3-20261126042522-46906dc3d97e", want: "v1.2.3-20261126042522-46906dc3d97e"},
		{name: "prerelease without zero segment", in: "v1.2.3-pre.1.20261126042522-46906dc3d97e", want: "v1.2.3-pre.1.20261126042522-46906dc3d97e"},
		{name: "short revision", in: "v0.9.3-0.20261126042522-abc1234", want: "dev"},
		{name: "invalid timestamp", in: "v0.9.3-0.99999999999999-46906dc3d97e", want: "dev"},

		{name: "dirty release tag", in: "v0.9.3+dirty", want: "dev"},
		{name: "dirty prerelease tag", in: "v0.9.3-rc.1+dirty", want: "dev"},
		{name: "dirty incompatible tag", in: "v2.0.0+incompatible.dirty", want: "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moduleReleaseVersion(tt.in); got != tt.want {
				t.Fatalf("moduleReleaseVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
