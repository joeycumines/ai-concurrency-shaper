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
		{name: "zero version", in: "v0.0.0-20261126042522-46906dc3d97e", want: "dev"},
		{name: "post tag", in: "v0.9.3-0.20261126042522-46906dc3d97e", want: "dev"},
		{name: "post tag dirty", in: "v0.9.3-0.20261126042522-46906dc3d97e+dirty", want: "dev"},
		{name: "prerelease pseudo", in: "v1.2.3-pre.0.20261126042522-46906dc3d97e", want: "dev"},
		{name: "base pseudo", in: "v1.2.3-20261126042522-46906dc3d97e", want: "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moduleReleaseVersion(tt.in); got != tt.want {
				t.Fatalf("moduleReleaseVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
