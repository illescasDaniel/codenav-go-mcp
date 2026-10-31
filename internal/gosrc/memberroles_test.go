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
