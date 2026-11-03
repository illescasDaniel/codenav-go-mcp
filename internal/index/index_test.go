package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeclarations(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/s.go", "package shapes\n\ntype Shape interface{ Area() float64 }\n\ntype Sq struct {\n\tSide float64 `json:\"side\"`\n\tShape\n}\n\nfunc (s *Sq) Area() float64 { return 0 }\n\nconst Unit, Half = 1, 2\n\nvar Big = \"é😀\"; var After = 1\n\nfunc Scale() {}\n")
	ix := New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	has := func(q string, kind, line, col int) {
		t.Helper()
		ex, _ := ix.Lookup(q)
		if len(ex) != 1 || ex[0].Kind != kind || ex[0].Line != line || ex[0].Col != col {
			t.Errorf("%s: %+v, want kind %d at %d:%d", q, ex, kind, line, col)
		}
	}
	has("Shape", KindInterface, 2, 5)
	has("Shape.Area", KindMethod, 2, 22)
	has("Sq", KindStruct, 4, 5)
	has("Sq.Side", KindField, 5, 1)
	has("Sq.Area", KindMethod, 9, 13)
	has("shapes.Sq.Area", KindMethod, 9, 13)
	has("Unit", KindConstant, 11, 6)
	has("Half", KindConstant, 11, 12)
	has("shapes.Scale", KindFunction, 15, 5)
	// "After" follows a multi-byte string on the same line: the column counts UTF-16 units
	// (é is one, the emoji two), not bytes.
	has("After", KindVariable, 13, len("var Big = \"é😀\"; var ")-len("é😀")+3)
}

func TestSkipsAndRefresh(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/a.go", "package a\n\nfunc One() {}\n")
	write(t, root, "vendor/v/v.go", "package v\n\nfunc One() {}\n")
	write(t, root, "testdata/t.go", "package t\n\nfunc One() {}\n")
	write(t, root, ".hidden/h.go", "package h\n\nfunc One() {}\n")
	write(t, root, "a/a_linux.go", "package a\n\nfunc Two() {}\n")
	write(t, root, "a/tag.go", "//go:build integration\n\npackage a\n\nfunc Three() {}\n")
	ix := New(root)
	_ = ix.Refresh()
	if ex, _ := ix.Lookup("One"); len(ex) != 1 {
		t.Errorf("only a/a.go declares One: %+v", ex)
	}
	if ex, _ := ix.Lookup("Two"); len(ex) != 1 || ex[0].Constraint != "file name suffix _linux" {
		t.Errorf("Two: %+v", ex)
	}
	if ex, _ := ix.Lookup("Three"); len(ex) != 1 || ex[0].Constraint != "//go:build integration" {
		t.Errorf("Three: %+v", ex)
	}
	// A new directory, a changed file and a deleted file are all picked up.
	write(t, root, "brandnew/b.go", "package brandnew\n\nfunc Fresh() {}\n")
	future := time.Now().Add(2 * time.Second)
	write(t, root, "a/a.go", "package a\n\nfunc Renamed() {}\n")
	_ = os.Chtimes(filepath.Join(root, "a/a.go"), future, future)
	_ = os.Remove(filepath.Join(root, "a/tag.go"))
	_ = ix.Refresh()
	if ex, _ := ix.Lookup("brandnew.Fresh"); len(ex) != 1 {
		t.Errorf("new directory: %+v", ex)
	}
	if ex, _ := ix.Lookup("One"); len(ex) != 0 {
		t.Errorf("One was renamed: %+v", ex)
	}
	if ex, _ := ix.Lookup("Renamed"); len(ex) != 1 {
		t.Errorf("Renamed: %+v", ex)
	}
	if ex, _ := ix.Lookup("Three"); len(ex) != 0 {
		t.Errorf("deleted file must leave the index: %+v", ex)
	}
}

func TestLookupExactBeatsManySimilar(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	b.WriteString("package many\n\nfunc U() {}\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "func U%d() {}\n", i)
	}
	write(t, root, "many/m.go", b.String())
	ix := New(root)
	_ = ix.Refresh()
	ex, loose := ix.Lookup("U")
	if len(ex) != 1 || len(loose) != 0 {
		t.Errorf("exact U: %d exact, %d loose", len(ex), len(loose))
	}
	if _, loose = ix.Lookup("u"); len(loose) != 1 {
		t.Errorf("a case-insensitive hit is loose: %d", len(loose))
	}
	if n := len(ix.Prefix("U")); n != 401 {
		t.Errorf("prefix U: %d", n)
	}
}

func TestUncompiledFilesAreSkipped(t *testing.T) {
	root := t.TempDir()
	put := func(name, src string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("ok.go", "package p\n\nfunc Seen() {}\n")
	put("_under.go", "package p\n\nfunc Under() {}\n")
	put(".dot.go", "package p\n\nfunc Dot() {}\n")
	put("ign.go", "//go:build ignore\n\npackage main\n\nfunc Ign() {}\n")
	put("ign2.go", "//go:build ignore && linux\n\npackage main\n\nfunc Ign2() {}\n")
	put("win.go", "//go:build !linux\n\npackage p\n\nfunc Win() {}\n")
	ix := New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"Seen": true, "Win": true, "Under": false, "Dot": false, "Ign": false, "Ign2": false} {
		if ex, _ := ix.Lookup(name); (len(ex) > 0) != want {
			t.Errorf("%s indexed = %v, want %v", name, len(ex) > 0, want)
		}
	}
	if !Unbuildable(filepath.Join(root, "ign.go")) || Unbuildable(filepath.Join(root, "win.go")) || !Unbuildable(filepath.Join(root, "_under.go")) {
		t.Error("Unbuildable disagrees with the index")
	}
}

// BUG-SCALE-001: a Refresh that stops at MaxFiles reports Capped; one that finishes under the cap does not.
func TestCappedAtMaxFiles(t *testing.T) {
	old := MaxFiles
	defer func() { MaxFiles = old }()
	root := t.TempDir()
	for _, f := range []string{"a/a.go", "b/b.go", "c/c.go"} {
		write(t, root, f, "package p\n\nfunc F() {}\n")
	}
	MaxFiles = 2
	ix := New(root)
	_ = ix.Refresh()
	if !ix.Capped() {
		t.Error("3 files with MaxFiles 2: the index must report it stopped at the cap")
	}
	MaxFiles = 3
	_ = ix.Refresh()
	if ix.Capped() {
		t.Error("3 files with MaxFiles 3: the index is complete and must not report a cap")
	}
}

// A //line directive changes the line and file names the parser reports, not the text. The index keeps the
// real position: a directive past the end of the file must not panic, a directive that renames the file or
// a huge line number must not move the declaration, and the declaration is listed once.
func TestLineDirectivesKeepRealPositions(t *testing.T) {
	cases := map[string]string{
		"past EOF":         "package p\n\n//line foo.y:10\nfunc fromY() int { return 3 }\n",
		"renames the file": "package p\n\n//line foo.y:2\nfunc fromY() int { return 3 }\n",
		"huge line":        "package p\n\n//line foo.y:1000000000\nfunc fromY() int { return 3 }\n", // go/scanner accepts up to 1<<30
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "p/gen.go", src)
			ix := New(root)
			if err := ix.Refresh(); err != nil {
				t.Fatal(err)
			}
			ex, loose := ix.Lookup("fromY")
			if len(ex) != 1 || len(loose) != 0 {
				t.Fatalf("fromY listed %d times (%d loose), want once: %+v", len(ex), len(loose), ex)
			}
			// The declaration is on line 4 (index 3), at column 6 (index 5), in the file that was read.
			if got := ex[0]; got.File != filepath.Join(root, "p/gen.go") || got.Line != 3 || got.Col != 5 {
				t.Errorf("fromY at %s %d:%d, want p/gen.go 3:5 (0-based)", got.File, got.Line, got.Col)
			}
		})
	}
}
