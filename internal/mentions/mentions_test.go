package mentions

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func kinds(ms []Mention) map[string]int {
	got := map[string]int{}
	for _, m := range ms {
		got[m.Kind]++
	}
	return got
}

func TestFind(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	// What the rename leaves: the declaration (and its doc comment) is already renamed.
	disk := "package p\n\n// Area is documented.\nfunc Area() {}\n\nvar s = \"Area\"\n\nfunc g() { Area() }\n"
	after := "package p\n\n// Box is documented.\nfunc Box() {}\n\nvar s = \"Area\"\n\nfunc g() { Area() }\n"
	_ = os.WriteFile(a, []byte(disk), 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("call Area here\nAreas are not\n"), 0o644)
	got := kinds(Find(root, "Area", map[string]string{a: after}, ""))
	want := map[string]int{"code": 1, "string": 1, "text": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %d want %d (%v)", k, got[k], v, got)
		}
	}
	if got["comment"] != 0 {
		t.Errorf("an already renamed doc comment must not be listed: %v", got)
	}
}

func TestFindShortName(t *testing.T) {
	root := t.TempDir()
	src := "package p\n\n// a value of the kind\nfunc f() {}\n\n// uses `a` and calls a(1)\nvar s = \"a\"\nvar t = \"a b\"\n"
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.go"), []byte("package p\n\n// a value\n"), 0o644)
	ms := Find(root, "a", nil, "")
	got := kinds(ms)
	if got["comment"] != 1 || got["string"] != 1 || got["code"] != 0 {
		t.Fatalf("short names need code context in comments and an exact string: %v\n%v", got, ms)
	}
	if only := Find(root, "a", nil, filepath.Join(root, "b.go")); len(only) != 0 {
		t.Fatalf("only=b.go: %v", only)
	}
}

func TestFindListsALineOnce(t *testing.T) {
	root := t.TempDir()
	src := "package p\n\ntype Map[T, U any] struct{ m map[T]U }\n\nfunc F[U any](u U) U { return u }\n"
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644)
	ms := Find(root, "U", nil, "")
	lines := map[int]int{}
	for _, m := range ms {
		lines[m.Line]++
	}
	for l, n := range lines {
		if n != 1 {
			t.Errorf("line %d listed %d times: %v", l, n, ms)
		}
	}
	if len(lines) != 2 {
		t.Errorf("want lines 3 and 5, got %v", lines)
	}
}

// A file that cannot refer to the symbol (codeIn false) but declares a same-named top-level name: its comments
// and strings are about that declaration, not leftovers of the rename.
func TestFindScopedSkipsFilesDeclaringTheName(t *testing.T) {
	root := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("decl.go", "package q\n\n// Widget is q's own.\nfunc Widget() {}\n\nvar s = \"Widget\"\n")
	write("declvar.go", "package q\n\n// Widget is q's own var.\nvar Widget = 1\n")
	write("decltype.go", "package q\n\n// Widget is q's own type.\ntype Widget struct{}\n")
	write("declmethod.go", "package q\n\n// Widget is a method here.\nfunc (T) Widget() {}\n\ntype T struct{}\n")
	write("talks.go", "package q\n\n// Widget is only talked about.\nvar s = \"Widget\"\n")
	write("refers.go", "package q\n\n// Widget is a real use.\nfunc g() { Widget() }\n")
	codeIn := func(path, text string) bool { return filepath.Base(path) == "refers.go" }
	files := map[string]bool{}
	for _, m := range FindScoped(root, "Widget", nil, "", codeIn) {
		files[m.Path] = true
	}
	for _, f := range []string{"decl.go", "declvar.go", "decltype.go", "declmethod.go"} {
		if files[f] {
			t.Errorf("%s declares its own Widget; its comments/strings must not be listed: %v", f, files)
		}
	}
	for _, f := range []string{"talks.go", "refers.go"} {
		if !files[f] {
			t.Errorf("%s must still be listed: %v", f, files)
		}
	}
}

// N1: a local variable or parameter that shadows the renamed package-level name is another object, not a
// leftover; a use of the package-level name is still listed.
func TestFindSkipsShadowingLocals(t *testing.T) {
	root := t.TempDir()
	src := "package p\n\nfunc g(foo int) int { return foo }\n\nfunc h() int {\n\tfoo := 5\n\tfoo++\n\treturn foo\n}\n\nfunc k() { foo() }\n"
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644)
	ms := Find(root, "foo", nil, "")
	var code []Mention
	for _, m := range ms {
		if m.Kind == "code" {
			code = append(code, m)
		}
	}
	if len(code) != 1 || code[0].Line != 11 {
		t.Fatalf("only the call foo() on line 11 is a leftover: %+v", code)
	}
}

// A //line directive past the end of the file must not move the reported lines of later code mentions:
// they are listed at their real lines, with the real text of those lines.
func TestFindIgnoresLineDirectives(t *testing.T) {
	root := t.TempDir()
	src := "package p\n\n//line foo.y:10\nfunc f() { Area() }\n\nfunc g() { Area() }\n"
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644)
	var got []string
	for _, m := range Find(root, "Area", nil, "") {
		if m.Kind == "code" {
			got = append(got, fmt.Sprintf("%d:%d %s", m.Line, m.Col, m.Snippet))
		}
	}
	want := []string{"4:12 func f() { Area() }", "6:12 func g() { Area() }"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("code mentions %v, want %v", got, want)
	}
}
