package tools

import (
	"reflect"
	"testing"
)

// NAVN-007: only a member of the cgo pseudo-package C is answered as a cgo name.
func TestCgoQualifiedNames(t *testing.T) {
	for q, want := range map[string]bool{
		"C.malloc":     true,
		"C.int":        true,
		"C":            false,
		"C.":           false,
		"Cx.malloc":    false,
		"pkg/C.malloc": false,
		"c.malloc":     false,
	} {
		if got := cgoQualified(q); got != want {
			t.Errorf("cgoQualified(%q) = %v, want %v", q, got, want)
		}
	}
}

// ESYM-008: a promoted method is named with the embedded type that declares it. promotedFrom walks the embedded
// fields of the type within its package directory, breadth first, and names the shallowest types that carry the
// member (hasMember says which do).
func TestPromotedFromNamesTheEmbeddedOwner(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p026/p.go": "package p026\n\ntype Inner struct{}\n\nfunc (Inner) M() int { return 1 }\n\ntype Outer struct{ Inner }\n",
		"p027/p.go": "package p027\n\ntype Inner struct{}\n\ntype Mid struct{ *Inner }\n\ntype Outer struct{ Mid }\n\ntype Plain struct{ x int }\n",
		"p028/p.go": "package p028\n\ntype Other struct{}\n\ntype Outer struct {\n\tfmt.Stringer\n\tOther\n}\n",
	})
	hasM := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, m := range names {
				if m == n {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name  string
		dir   string
		typ   string
		carry func(string) bool
		want  []string
	}{
		{"direct embedding", dir + "/p026", "Outer", hasM("Inner"), []string{"Inner"}},
		{"through a pointer-embedded intermediate", dir + "/p027", "Outer", hasM("Inner"), []string{"Inner"}},
		{"no embedded type carries the member", dir + "/p026", "Outer", hasM("Nobody"), nil},
		{"type without embedded fields", dir + "/p027", "Plain", hasM("Inner"), nil},
		{"the type itself is not its own owner", dir + "/p026", "Inner", hasM("Inner"), nil},
		{"a same-package embedded type is named", dir + "/p028", "Outer", hasM("Other"), []string{"Other"}},
		{"a qualified embedded type is not named", dir + "/p028", "Outer", hasM("Stringer"), nil},
	}
	for _, c := range cases {
		if got := promotedFrom(c.dir, c.typ, c.carry); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: promotedFrom(%s) = %v, want %v", c.name, c.typ, got, c.want)
		}
	}
}
