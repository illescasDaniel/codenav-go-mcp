package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// declShapeAt tells the package-level names apart from the locals, parameters, fields and interface methods that
// share a spelling with them, so that buildGuard only checks what the build can leave out.
func TestDeclShapeAt(t *testing.T) {
	const src = `package p

type T struct{ Name int }

type I interface{ Name() int }

var V = 1

const C = 2

func F(Name int) int {
	Name2 := Name
	return Name2
}

func Ext(x int) int

func (T) M() int { return 0 }
`
	// at is the byte offset of the nth (0-based) occurrence of name in src.
	at := func(name string, nth int) int {
		t.Helper()
		off, from := -1, 0
		for i := 0; i <= nth; i++ {
			k := strings.Index(src[from:], name)
			if k < 0 {
				t.Fatalf("no %s", name)
			}
			off = from + k
			from = off + len(name)
		}
		return off
	}
	cases := []struct {
		label string
		off   int
		ok    bool
		shape declShape
	}{
		{"type T", at("T", 0), true, declShape{}},
		{"struct field Name", at("Name", 0), false, declShape{}},
		{"interface method Name", at("Name", 1), true, declShape{method: true}},
		{"var V", at("V", 0), true, declShape{}},
		{"const C", at("C", 0), true, declShape{}},
		{"func F", at("F", 0), true, declShape{}},
		{"parameter Name of F", at("Name", 2), false, declShape{}},
		{"local Name2 of F", at("Name2", 0), false, declShape{}},
		{"body-less func Ext", at("Ext", 0), true, declShape{bodyless: true}},
		{"method M", at("M", 0), true, declShape{method: true}},
	}
	for _, tc := range cases {
		shape, ok := declShapeAt("p.go", src, tc.off)
		if ok != tc.ok || shape != tc.shape {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", tc.label, shape, ok, tc.shape, tc.ok)
		}
	}
}

// linknameSites finds the directives that name a package-level name: a local one in the declaring directory, and a
// pull from another package (its target is importPath.name), and nothing else.
func TestLinknameSites(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("p/p.go", "package p\n\nimport _ \"unsafe\"\n\n//go:linkname hidden\nfunc hidden(x int) int { return x }\n\n//go:linkname other\nfunc other() {}\n")
	write("q/q.go", "package q\n\nimport _ \"unsafe\"\n\n//go:linkname hiddenRef example.com/m/p.hidden\nfunc hiddenRef(x int) int\n\n//go:linkname elsewhere example.com/m/r.hidden\nfunc elsewhere(x int) int\n")
	write("q/doc.go", "package q\n\n// The directive //go:linkname hidden in a comment is not one.\n")
	declFile := filepath.Join(root, "p/p.go")

	got := linknameSites(root, declFile, "hidden", "example.com/m/p")
	want := []string{"p/p.go:5 (//go:linkname hidden)", "q/q.go:5 (//go:linkname hiddenRef example.com/m/p.hidden)"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("linknameSites:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := linknameSites(root, declFile, "hidden", ""); len(got) != 1 {
		t.Fatalf("without an import path only the local directive counts: %v", got)
	}
}
