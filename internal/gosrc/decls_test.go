package gosrc

import (
	"strings"
	"testing"
)

const sample = `package p

import (
	"fmt"
	"strings"
)

// A is documented.
func A() { fmt.Println() }

type T struct{ X int }

const (
	// One is first.
	One = iota // trailing
	Two
)

var a, b = 1, 2
`

func at(t *testing.T, name string) *Decl {
	t.Helper()
	i := strings.Index(sample, name)
	if i < 0 {
		t.Fatalf("%s not in sample", name)
	}
	d, err := FindDeclAt("p.go", sample, i)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFindDeclAt(t *testing.T) {
	d := at(t, "A()")
	if !d.HasDoc || !strings.HasPrefix(sample[d.Start:], "// A is documented.") || !strings.HasSuffix(sample[:d.End], "Println() }") {
		t.Errorf("func span wrong: %q", sample[d.Start:d.End])
	}
	d = at(t, "T struct")
	if d.Keyword != "type" || d.BodyKind != "struct" || sample[d.Closing] != '}' {
		t.Errorf("type decl wrong: %+v", d)
	}
	d = at(t, "One =")
	if !d.Grouped || sample[d.Start:d.End] != "// One is first.\n\tOne = iota // trailing" {
		t.Errorf("grouped spec span wrong: %q", sample[d.Start:d.End])
	}
	if !at(t, "a, b").MultiName {
		t.Error("a, b should be MultiName")
	}
	if _, err := FindDeclAt("p.go", sample, strings.Index(sample, "X int")); err == nil {
		t.Error("a struct field is not a top-level declaration")
	}
}

func TestEditImports(t *testing.T) {
	next := strings.Replace(sample, "fmt.Println()", "", 1)
	got, err := EditImports("p.go", next, sample, []string{`js "encoding/json"`}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"fmt"`) {
		t.Errorf("fmt should be pruned:\n%s", got)
	}
	if !strings.Contains(got, `js "encoding/json"`) {
		t.Errorf("alias import missing:\n%s", got)
	}
	if same, _ := EditImports("p.go", sample, sample, nil, true); same != sample {
		t.Error("nothing to change must return the source untouched")
	}
}

func TestDeclNames(t *testing.T) {
	got, err := DeclNames("func (s *S) M() {}\nfunc F() {}\nvar x = 1\ntype Y int")
	if err != nil || strings.Join(got, ",") != "S.M,F,x,Y" {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestCarveSpec(t *testing.T) {
	carve := func(src, name string) (string, error) {
		off := strings.Index(src, name)
		out, noff, err := CarveSpec("x.go", src, off)
		if err == nil && out[noff:noff+len(name)] != name {
			t.Fatalf("returned offset does not point at %s: %q", name, out[noff:])
		}
		return out, err
	}
	ok := []struct{ name, src, sym, want string }{
		{"group", "package p\n\nconst (\n\tA = 1\n\tB = 2\n)\n", "B", "const B = 2"},
		{"typed multi", "package p\n\nvar a, b int = 1, 2\n", "b", "var b int = 2"},
		{"multi in group", "package p\n\nvar (\n\tx = 1\n\ta, b = 1, 2\n)\n", "b", "var b = 2"},
		{"no values", "package p\n\nvar x, y int\n", "x", "var x int"},
	}
	for _, c := range ok {
		got, err := carve(c.src, c.sym)
		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("%s: %v\n%s", c.name, err, got)
		}
	}
	bad := []struct{ name, src, sym string }{
		{"multi-value call", "package p\n\nvar u, v = f()\n\nfunc f() (int, int) { return 1, 2 }\n", "u"},
		{"iota", "package p\n\nconst (\n\tA = iota\n\tB = 5\n)\n", "B"},
		{"implicit next", "package p\n\nconst (\n\tA = 1\n\tB\n)\n", "A"},
		{"shared line", "package p\n\nconst (\n\tA = 1; B = 2\n)\n", "B"},
	}
	for _, c := range bad {
		if _, err := carve(c.src, c.sym); err == nil {
			t.Errorf("%s must be refused", c.name)
		}
	}
}

func TestEditImportsCollapsesSingleImport(t *testing.T) {
	old := "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\nfunc F() string { fmt.Println(); return strings.ToUpper(\"a\") }\n"
	next := strings.Replace(old, "fmt.Println(); ", "", 1)
	got, err := EditImports("p.go", next, old, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "import \"strings\"\n") || strings.Contains(got, "import (") {
		t.Errorf("single import should collapse:\n%s", got)
	}
}
