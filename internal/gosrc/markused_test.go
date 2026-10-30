package gosrc

import (
	"sort"
	"strings"
	"testing"
)

// apply inserts every Insertion (offsets refer to src).
func apply(src string, ins []Insertion) string {
	sort.SliceStable(ins, func(i, j int) bool { return ins[i].Offset < ins[j].Offset })
	var sb strings.Builder
	cur := 0
	for _, in := range ins {
		sb.WriteString(src[cur:in.Offset])
		sb.WriteString(in.Text)
		cur = in.Offset
	}
	sb.WriteString(src[cur:])
	return sb.String()
}

func TestMarkUsed(t *testing.T) {
	cases := []struct {
		name, src, v string // v: the variable; its first occurrence after "marker" is the declaration
		nth          int    // which occurrence of v in src is the declaration (0-based)
		want         string
	}{
		{"plain", "package p\n\nfunc f() {\n\tx := 1\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n}\n"},
		{"trailing comment stays", "package p\n\nfunc f() {\n\tx := 1 // keep\n\tg()\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tx := 1 // keep\n\t_ = x\n\tg()\n}\n"},
		{"var decl", "package p\n\nfunc f() {\n\tvar x int\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tvar x int\n\t_ = x\n}\n"},
		{"multi-line statement", "package p\n\nfunc f() {\n\tx := g(1,\n\t\t2)\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tx := g(1,\n\t\t2)\n\t_ = x\n}\n"},
		{"one-line block", "package p\n\nfunc f() {\n\tif true { x := 1 }\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tif true { x := 1; _ = x }\n}\n"},
		{"shadowing only the inner", "package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n\t{\n\t\tx := 2\n\t}\n}\n", "x", 2,
			"package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n\t{\n\t\tx := 2\n\t\t_ = x\n\t}\n}\n"},
		{"closure", "package p\n\nfunc f() {\n\tg := func() {\n\t\tx := 1\n\t}\n\tg()\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tg := func() {\n\t\tx := 1\n\t\t_ = x\n\t}\n\tg()\n}\n"},
		{"if init", "package p\n\nfunc f() {\n\tif x := g(); true {\n\t\th()\n\t}\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tif x := g(); true {\n\t\t_ = x\n\t\th()\n\t}\n}\n"},
		{"for init empty body", "package p\n\nfunc f() {\n\tfor i := 0; ; {\n\t}\n}\n", "i", 0,
			"package p\n\nfunc f() {\n\tfor i := 0; ; {\n\t\t_ = i\n\t}\n}\n"},
		{"range var, braces on one line", "package p\n\nfunc f(xs []int) {\n\tfor i := range xs {}\n}\n", "i", 0,
			"package p\n\nfunc f(xs []int) {\n\tfor i := range xs {\n\t\t_ = i\n\t}\n}\n"},
		{"switch init, every clause", "package p\n\nfunc f() {\n\tswitch x := g(); {\n\tcase true:\n\t\th()\n\tdefault:\n\t}\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tswitch x := g(); {\n\tcase true:\n\t\t_ = x\n\t\th()\n\tdefault:\n\t\t_ = x\n\t}\n}\n"},
		{"type switch binding", "package p\n\nfunc f(a any) {\n\tswitch v := a.(type) {\n\tcase int:\n\tcase string:\n\t\th()\n\t}\n}\n", "v", 0,
			"package p\n\nfunc f(a any) {\n\tswitch v := a.(type) {\n\tcase int:\n\t\t_ = v\n\tcase string:\n\t\t_ = v\n\t\th()\n\t}\n}\n"},
		{"select case", "package p\n\nfunc f(c chan int) {\n\tselect {\n\tcase v := <-c:\n\t\th()\n\t}\n}\n", "v", 0,
			"package p\n\nfunc f(c chan int) {\n\tselect {\n\tcase v := <-c:\n\t\t_ = v\n\t\th()\n\t}\n}\n"},
		{"case body", "package p\n\nfunc f() {\n\tswitch {\n\tcase true:\n\t\tx := 1\n\t}\n}\n", "x", 0,
			"package p\n\nfunc f() {\n\tswitch {\n\tcase true:\n\t\tx := 1\n\t\t_ = x\n\t}\n}\n"},
		{"second of two", "package p\n\nfunc f() {\n\ta, b := 1, 2\n\t_ = a\n}\n", "b", 0,
			"package p\n\nfunc f() {\n\ta, b := 1, 2\n\t_ = b\n\t_ = a\n}\n"},
	}
	for _, c := range cases {
		off := -1
		for i, from := 0, 0; i <= c.nth; i++ {
			j := strings.Index(c.src[from:], c.v)
			// skip occurrences inside longer identifiers (e.g. "xs", "func")
			for j >= 0 && isIdentByte(c.src, from+j+len(c.v)) || j >= 0 && from+j > 0 && isIdentByte(c.src, from+j-1) {
				from += j + 1
				j = strings.Index(c.src[from:], c.v)
			}
			off = from + j
			from = off + 1
		}
		ins, err := MarkUsed(c.src, off, c.v)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := apply(c.src, ins); got != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
		if err := CheckSyntax("x.go", apply(c.src, ins)); err != nil {
			t.Errorf("%s: result does not parse: %v", c.name, err)
		}
	}
}

func isIdentByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	b := s[i]
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func TestMarkUsedRefusesParameters(t *testing.T) {
	src := "package p\n\nfunc f(x int) {}\n"
	if _, err := MarkUsed(src, strings.Index(src, "x"), "x"); err == nil {
		t.Fatal("a parameter is not a local declaration")
	}
}
