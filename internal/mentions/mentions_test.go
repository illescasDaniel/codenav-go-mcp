package mentions

import (
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
