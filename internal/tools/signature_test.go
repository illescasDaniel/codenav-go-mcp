package tools

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
)

// Nested calls f(f(x), y): the inner rewrite must survive inside the outer one.
func TestRenderNestedCalls(t *testing.T) {
	src := "package p\n\nfunc f(a, b int) int { return a }\n\nvar _ = f(f(1, 2), 3)\n"
	pf, err := gosrc.ParseFile("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	build := func(args []string) string { return args[1] + ", " + args[0] }
	var es []sigEdit
	for _, needle := range []string{"f(f(", "f(1"} {
		off := strings.Index(src, needle)
		if needle == "f(f(" {
			off = strings.Index(src, "f(f(")
		}
		c := pf.CallAt(off)
		if c == nil {
			t.Fatalf("no call at %q", needle)
		}
		es = append(es, sigEdit{start: c.Start, end: c.End, args: c.Args, build: build})
	}
	got := render(src, 0, len(src), es)
	if !strings.Contains(got, "f(3, f(2, 1))") {
		t.Errorf("got %q", got)
	}
}

func TestParamList(t *testing.T) {
	have := []gosrc.Param{{Name: "a", Type: "int"}, {Name: "b", Type: "int"}, {Name: "rest", Type: "...string", Variadic: true}}
	got := paramList(have, []entry{{old: 0}, {old: 1}, {old: -1, add: &addParam{Name: "c", Type: "int"}}, {old: 2}})
	if got != "a, b, c int, rest ...string" {
		t.Errorf("got %q", got)
	}
}
