package transcode

// chooseToolFlatName keeps the map INVERTIBLE: every flat name it hands out maps
// back to exactly the ref it was chosen for, even when a plain function reserves
// the name a child would otherwise take, and even when a qualified name is itself
// taken. A flat name that reverse-mapped to the wrong child would let a replayed
// call be attributed to a different tool, which no policy can approve.

import (
	"testing"
)

// flatNameInversion drives chooseToolFlatName over a declaration order and
// asserts every mapping is one-to-one in both directions.
func assertToolNamesInvertible(t *testing.T, names *ToolNames) {
	t.Helper()
	for ref, flat := range names.RefToFlat {
		back, ok := names.FlatToRef[flat]
		if !ok {
			t.Fatalf("RefToFlat[%+v] = %q has no FlatToRef entry", ref, flat)
		}
		if back != ref {
			t.Fatalf("flat name %q reverse-maps to %+v, want %+v", flat, back, ref)
		}
	}
	for flat, ref := range names.FlatToRef {
		forward, ok := names.RefToFlat[ref]
		if !ok {
			t.Fatalf("FlatToRef[%q] = %+v has no RefToFlat entry", flat, ref)
		}
		if forward != flat {
			t.Fatalf("ref %+v maps forward to %q, want %q", ref, forward, flat)
		}
	}
	if len(names.RefToFlat) != len(names.FlatToRef) {
		t.Fatalf("RefToFlat has %d entries and FlatToRef %d: the map is not one-to-one",
			len(names.RefToFlat), len(names.FlatToRef))
	}
}

// TestChooseToolFlatNameStaysInvertible proves the map is one-to-one across the
// shapes that force qualification: a plain-name reservation, a child colliding
// with an earlier child, and a qualified name that is itself reserved.
func TestChooseToolFlatNameStaysInvertible(t *testing.T) {
	cases := []struct {
		name     string
		reserved []string
		children []ToolNameRef
	}{
		{
			name:     "bare names are unique",
			children: []ToolNameRef{{Namespace: "ns", Name: "a"}, {Namespace: "ns", Name: "b"}},
		},
		{
			name:     "plain function reserves a child name",
			reserved: []string{"foo"},
			children: []ToolNameRef{{Namespace: "ns", Name: "foo"}},
		},
		{
			name:     "two namespaces share a child name",
			children: []ToolNameRef{{Namespace: "a", Name: "f"}, {Namespace: "b", Name: "f"}},
		},
		{
			name:     "a qualified name is reserved",
			reserved: []string{"ns__foo"},
			children: []ToolNameRef{{Namespace: "ns", Name: "foo"}},
		},
		{
			name: "a child's literal name collides with an earlier qualified name",
			children: []ToolNameRef{
				{Namespace: "ns1", Name: "foo"},
				{Namespace: "ns2", Name: "ns1__foo"},
			},
		},
		{
			name:     "reserved bare and reserved qualified at once",
			reserved: []string{"foo", "ns__foo", "ns__foo_2"},
			children: []ToolNameRef{{Namespace: "ns", Name: "foo"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reserved := make(map[string]struct{}, len(tc.reserved))
			for _, name := range tc.reserved {
				reserved[name] = struct{}{}
			}
			names := &ToolNames{FlatToRef: map[string]ToolNameRef{}, RefToFlat: map[ToolNameRef]string{}}
			for _, child := range tc.children {
				flat := chooseToolFlatName(names, reserved, child.Namespace, child.Name)
				// A flat name must never collide with a reserved plain name.
				if _, taken := reserved[flat]; taken {
					t.Fatalf("flat name %q is already reserved by a plain function", flat)
				}
			}
			assertToolNamesInvertible(t, names)
		})
	}
}

// TestChooseToolFlatNameIsOrderStable proves re-asking for a known ref returns
// the SAME flat name, which is what keeps a replay consistent across calls.
func TestChooseToolFlatNameIsOrderStable(t *testing.T) {
	names := &ToolNames{FlatToRef: map[string]ToolNameRef{}, RefToFlat: map[ToolNameRef]string{}}
	reserved := map[string]struct{}{"f": {}}
	first := chooseToolFlatName(names, reserved, "ns", "f")
	second := chooseToolFlatName(names, reserved, "ns", "f")
	if first != second {
		t.Fatalf("chooseToolFlatName returned %q then %q for one ref", first, second)
	}
	if len(names.RefToFlat) != 1 || len(names.FlatToRef) != 1 {
		t.Fatalf("re-asking for a ref added an entry: %+v / %+v", names.RefToFlat, names.FlatToRef)
	}
}
