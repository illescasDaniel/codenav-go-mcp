package gosrc

import (
	"strings"
	"testing"
)

func TestNonMemberIdents(t *testing.T) {
	src := "package p\n\ntype T struct{ Area int }\n\nfunc (T) Area2() {}\n\nfunc Area() {}\n\nfunc g(t T) { Area(); _ = t.Area; _ = T{Area: 1} }\n"
	got := NonMemberIdents("p.go", src, "Area")
	if !got[7<<20|6] || !got[9<<20|15] || len(got) != 2 {
		t.Errorf("only the func declaration and the bare call are non-members: %v", got)
	}
	sel := NonSelectorIdents("p.go", src, "Area")
	if sel[9<<20|30] || !sel[9<<20|15] {
		t.Errorf("a selected name is not bare: %v", sel)
	}
}

func TestDropOrphanImportComments(t *testing.T) {
	before := "package p\n\nimport (\n\t// Std\n\t\"fmt\"\n\n\t// Gone\n\t\"os\" // tail\n\n\t// kept note\n)\n"
	after := "package p\n\nimport (\n\t// Std\n\t\"fmt\"\n\t// Gone\n\t// tail\n\t// kept note\n)\n"
	got := DropOrphanImportComments("p.go", before, after)
	want := "package p\n\nimport (\n\t// Std\n\t\"fmt\"\n\t// kept note\n)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDropOrphanImportCommentsHeadings(t *testing.T) {
	before := "package p\n\nimport (\n\t// Std\n\t\"fmt\"\n\t\"os\"\n\t// Third party\n\t\"x/y\"\n\t// Trailing\n)\n"
	after := "package p\n\nimport (\n\t// Std\n\t\"fmt\"\n\t// Third party\n\t\"x/y\"\n\t// Trailing\n)\n"
	got := DropOrphanImportComments("p.go", before, after)
	if !strings.Contains(got, "// Std") || !strings.Contains(got, "\"x/y\"") || strings.Contains(got, "Trailing") {
		t.Errorf("trailing heading must go:\n%s", got)
	}
}

// Round 13 (item 9): the type checker tells a field, selector, keyed literal or method named like a
// package-level symbol from the symbol itself.
func TestMemberIdentsInDir(t *testing.T) {
	a := "package p\n\nimport \"fmt\"\n\nfunc foo(n int) int { return n }\n\ntype T struct{ foo int }\n\nfunc (t T) Use() int { return t.foo + foo(1) }\n\nfunc (t T) bar() {}\n\nvar _ = T{foo: 1}\n\nvar _ = map[string]int{\"x\": foo(2)}\n\nfunc F() { fmt.Println(foo(3)) }\n"
	b := "package p\n\nfunc (t T) foo2() {}\n\nfunc Other(t T) int { return t.foo }\n"
	got := MemberIdentsInDir(map[string]string{"/d/a.go": a, "/d/b.go": b}, "foo")
	ga, gb := got["/d/a.go"], got["/d/b.go"]
	// Members: the field declaration (7:16), t.foo (9:33), the keyed literal (13:11), t.foo in b.go (5:32).
	for _, want := range []int{7<<20 | 16, 9<<20 | 33, 13<<20 | 11} {
		if !ga[want] {
			t.Errorf("a.go: %d:%d should be a member: %v", want>>20, want&(1<<20-1), ga)
		}
	}
	if !gb[5<<20|32] {
		t.Errorf("b.go: t.foo (a field declared in another file) should be a member: %v", gb)
	}
	// Not members: the func declaration (5:6), its calls, including the call in a map literal.
	for _, no := range []int{5<<20 | 6, 9<<20 | 39, 15<<20 | 29, 17<<20 | 24} {
		if ga[no] {
			t.Errorf("a.go: %d:%d is the package-level func, not a member", no>>20, no&(1<<20-1))
		}
	}
	// A method named like the symbol is a member too.
	m := MemberIdentsInDir(map[string]string{"/d/m.go": "package p\n\nfunc foo() {}\n\ntype T struct{}\n\nfunc (T) foo() {}\n"}, "foo")
	if !m["/d/m.go"][7<<20|10] || m["/d/m.go"][3<<20|6] {
		t.Errorf("method vs func: %v", m)
	}
}

// A //line directive does not move the keys: they are the real line and column of each identifier.
func TestNonMemberIdentsIgnoresLineDirectives(t *testing.T) {
	got := NonMemberIdents("p.go", "package p\n//line x.go:1\nfunc Area() {}\nvar v = Area\n", "Area")
	if !got[3<<20|6] || !got[4<<20|9] || len(got) != 2 {
		t.Errorf("Area at 3:6 and 4:9 expected, got %v", got)
	}
}
