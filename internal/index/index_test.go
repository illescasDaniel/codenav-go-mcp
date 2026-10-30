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
