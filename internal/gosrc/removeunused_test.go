package gosrc

import (
	"sort"
	"strings"
	"testing"
)

func applyRepl(src string, rs []Replacement) string {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Start < rs[j].Start })
	var sb strings.Builder
	cur := 0
	for _, r := range rs {
		sb.WriteString(src[cur:r.Start])
		sb.WriteString(r.Text)
		cur = r.End
	}
	sb.WriteString(src[cur:])
	return sb.String()
}

func nthIdent(src, v string, nth int) int {
	off := -1
	for i, from := 0, 0; i <= nth; i++ {
		j := strings.Index(src[from:], v)
		for j >= 0 && (isIdentByte(src, from+j+len(v)) || from+j > 0 && isIdentByte(src, from+j-1)) {
			from += j + 1
			j = strings.Index(src[from:], v)
		}
		off = from + j
		from = off + 1
	}
	return off
}

func TestRemoveUnused(t *testing.T) {
	const h = "package p\n\nfunc f() {\n"
	cases := []struct {
		name, src, v string
		nth          int
		want         string
	}{
		{"plain literal", h + "\tx := 1\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"trailing comment goes with it", h + "\tx := 1 // note\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"pure: selector, composite, conversion", h + "\tx := []byte(a.b)\n\ty := T{A: 1, B: []int{2}}\n\tg()\n}\n", "x", 0, h + "\ty := T{A: 1, B: []int{2}}\n\tg()\n}\n"},
		{"call is kept", h + "\tx := g(1)\n}\n", "x", 0, h + "\tg(1)\n}\n"},
		{"receive is kept", h + "\tx := <-c\n}\n", "x", 0, h + "\t<-c\n}\n"},
		{"call inside an expression", h + "\tx := g() + 1\n}\n", "x", 0, h + "\t_ = g() + 1\n}\n"},
		{"conversion of a call", h + "\tx := int(g())\n}\n", "x", 0, h + "\t_ = int(g())\n}\n"},
		{"builtin len", h + "\tx := len(s)\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"index may panic: kept", h + "\tx := s[i]\n}\n", "x", 0, h + "\t_ = s[i]\n}\n"},
		{"multi-assign keeps :=", h + "\ta, x := g()\n\tuse(a)\n}\n", "x", 0, h + "\ta, _ := g()\n\tuse(a)\n}\n"},
		{"multi-assign, others existing: =", h + "\tvar a int\n\ta, x := g()\n\tuse(a)\n}\n", "x", 1, h + "\tvar a int\n\ta, _ = g()\n\tuse(a)\n}\n"},
		{"multi-assign, all others blank", h + "\t_, x := g()\n}\n", "x", 0, h + "\t_, _ = g()\n}\n"},
		{"multi-assign, parameter is existing", "package p\n\nfunc f(a int) {\n\ta, x := g()\n\tuse(a)\n}\n", "x", 0, "package p\n\nfunc f(a int) {\n\ta, _ = g()\n\tuse(a)\n}\n"},
		{"multi-assign in a new scope declares a again", h + "\tvar a int\n\tuse(a)\n\t{\n\t\ta, x := g()\n\t\tuse(a)\n\t}\n}\n", "x", 0, h + "\tvar a int\n\tuse(a)\n\t{\n\t\ta, _ := g()\n\t\tuse(a)\n\t}\n}\n"},
		{"var decl", h + "\tvar x int\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"var decl with value", h + "\tvar x int = 4\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"var decl with call", h + "\tvar x int = g()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"var group member", h + "\tvar (\n\t\ta int\n\t\tx int\n\t)\n\tuse(a)\n}\n", "x", 0, h + "\tvar (\n\t\ta int\n\t)\n\tuse(a)\n}\n"},
		{"var group call keeps blank", h + "\tvar (\n\t\ta int\n\t\tx = g()\n\t)\n\tuse(a)\n}\n", "x", 0, h + "\tvar (\n\t\ta int\n\t\t_ = g()\n\t)\n\tuse(a)\n}\n"},
		{"var group emptied", h + "\tvar (\n\t\tx int\n\t)\n\tg()\n}\n", "x", 0, h + "\tg()\n}\n"},
		{"var names", h + "\tvar a, x int\n\tuse(a)\n}\n", "x", 0, h + "\tvar a int\n\tuse(a)\n}\n"},
		{"var first name", h + "\tvar x, a int\n\tuse(a)\n}\n", "x", 0, h + "\tvar a int\n\tuse(a)\n}\n"},
		{"var names with call", h + "\tvar a, x = g()\n\tuse(a)\n}\n", "x", 0, h + "\tvar a, _ = g()\n\tuse(a)\n}\n"},
		{"closure only that declaration", h + "\tx := 1\n\tf := func() {\n\t\tx := 2\n\t}\n\tf()\n\tuse(x)\n}\n", "x", 1, h + "\tx := 1\n\tf := func() {\n\t}\n\tf()\n\tuse(x)\n}\n"},
		{"one-line block", h + "\tif c { x := 1; g() }\n}\n", "x", 0, h + "\tif c { g() }\n}\n"},
		{"if init pure", h + "\tif x := 1; c {\n\t\tg()\n\t}\n}\n", "x", 0, h + "\tif c {\n\t\tg()\n\t}\n}\n"},
		{"if init call", h + "\tif x := g(); c {\n\t\th()\n\t}\n}\n", "x", 0, h + "\tif g(); c {\n\t\th()\n\t}\n}\n"},
		{"if init two vars", h + "\tif a, x := g(); a {\n\t\th()\n\t}\n}\n", "x", 0, h + "\tif a, _ := g(); a {\n\t\th()\n\t}\n}\n"},
		{"switch init", h + "\tswitch x := g(); {\n\tcase c:\n\t}\n}\n", "x", 0, h + "\tswitch g(); {\n\tcase c:\n\t}\n}\n"},
		{"switch init pure with tag", h + "\tswitch x := 1; y {\n\t}\n}\n", "x", 0, h + "\tswitch y {\n\t}\n}\n"},
		{"for init", h + "\tfor x := 0; c; {\n\t}\n}\n", "x", 0, h + "\tfor ; c; {\n\t}\n}\n"},
		{"range value", h + "\tfor i, x := range xs {\n\t\tuse(i)\n\t}\n}\n", "x", 0, h + "\tfor i := range xs {\n\t\tuse(i)\n\t}\n}\n"},
		{"range key with value", h + "\tfor i, v := range xs {\n\t\tuse(v)\n\t}\n}\n", "i", 0, h + "\tfor _, v := range xs {\n\t\tuse(v)\n\t}\n}\n"},
		{"range key alone", h + "\tfor i := range xs {\n\t}\n}\n", "i", 0, h + "\tfor range xs {\n\t}\n}\n"},
		{"type switch binding", h + "\tswitch v := a.(type) {\n\tcase int:\n\t}\n}\n", "v", 0, h + "\tswitch a.(type) {\n\tcase int:\n\t}\n}\n"},
		{"select receive", h + "\tselect {\n\tcase v := <-c:\n\t\tg()\n\t}\n}\n", "v", 0, h + "\tselect {\n\tcase <-c:\n\t\tg()\n\t}\n}\n"},
		{"case body", h + "\tswitch {\n\tcase c:\n\t\tx := 1\n\t\tg()\n\t}\n}\n", "x", 0, h + "\tswitch {\n\tcase c:\n\t\tg()\n\t}\n}\n"},
		{"CRLF", "package p\r\n\r\nfunc f() {\r\n\tx := 1\r\n\tg()\r\n}\r\n", "x", 0, "package p\r\n\r\nfunc f() {\r\n\tg()\r\n}\r\n"},
	}
	for _, c := range cases {
		rs, err := RemoveUnused(c.src, nthIdent(c.src, c.v, c.nth), c.v)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := applyRepl(c.src, rs)
		if got != c.want {
			t.Errorf("%s:\n got:\n%q\nwant:\n%q", c.name, got, c.want)
		}
		if err := CheckSyntax("x.go", got); err != nil {
			t.Errorf("%s: result does not parse: %v", c.name, err)
		}
	}
}

func TestRemoveUnusedRefuses(t *testing.T) {
	cases := map[string]string{
		"parameter":         "package p\n\nfunc f(x int) {}\n",
		"result":            "package p\n\nfunc f() (x int) { return }\n",
		"labelled":          "package p\n\nfunc f() {\nL:\n\tx := 1\n\tgoto L\n}\n",
		"closure parameter": "package p\n\nfunc f() {\n\tg := func(x int) {}\n\tg(1)\n}\n",
	}
	for name, src := range cases {
		i := strings.Index(src, "x")
		if name == "labelled" {
			i = strings.Index(src, "x :=")
		}
		if _, err := RemoveUnused(src, i, "x"); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}
