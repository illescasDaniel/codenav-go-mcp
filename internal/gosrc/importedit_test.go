package gosrc

import (
	"strings"
	"testing"
)

func pruneAfter(t *testing.T, old, cut string) string {
	t.Helper()
	next := strings.Replace(old, cut, "", 1)
	if next == old {
		t.Fatalf("%q not in source", cut)
	}
	got, err := EditImports("p.go", next, old, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPruneDotImport(t *testing.T) {
	old := "package p\n\nimport (\n\t\"fmt\"\n\t. \"math\"\n\t_ \"embed\"\n)\n\nfunc F() float64 { fmt.Println(); return Sqrt(2) }\n"
	got := pruneAfter(t, old, "return Sqrt(2)")
	if strings.Contains(got, "math") || !strings.Contains(got, `_ "embed"`) {
		t.Errorf("the unused dot import goes, the blank one stays:\n%s", got)
	}
	// still used: kept
	if got := pruneAfter(t, old, "fmt.Println(); "); !strings.Contains(got, `. "math"`) || strings.Contains(got, `"fmt"`) {
		t.Errorf("a used dot import stays:\n%s", got)
	}
}

func TestPruneDuplicateImportByLocalName(t *testing.T) {
	old := "package p\n\nimport (\n\t\"fmt\"\n\tf2 \"fmt\"\n)\n\nfunc F() { fmt.Println(); f2.Println() }\n"
	got := pruneAfter(t, old, "fmt.Println(); ")
	if !strings.Contains(got, `f2 "fmt"`) || strings.Contains(got, "\t\"fmt\"") {
		t.Errorf("only the plain fmt goes:\n%s", got)
	}
	got = pruneAfter(t, old, "; f2.Println()")
	if strings.Contains(got, "f2") || !strings.Contains(got, `"fmt"`) {
		t.Errorf("only the alias goes:\n%s", got)
	}
}

func TestPruneTakesCommentsAndEmptyGroupWithIt(t *testing.T) {
	old := "package p\n\nimport (\n\t\"fmt\" // trailing fmt\n\t\"os\"\n\n\t// third-party\n\t\"github.com/x/y\" // trailing y\n)\n\nfunc F() { fmt.Println(); os.Exit(1); y.Do() }\n"
	got := pruneAfter(t, old, "y.Do()")
	if want := "import (\n\t\"fmt\" // trailing fmt\n\t\"os\"\n)\n"; !strings.Contains(got, want) || strings.Contains(got, "third-party") || strings.Contains(got, "trailing y") {
		t.Errorf("spec, its comments and the empty group go:\n%s", got)
	}
	got = pruneAfter(t, old, "fmt.Println(); ")
	if strings.Contains(got, "trailing fmt") || !strings.Contains(got, "\t\"os\"\n\n\t// third-party\n\t\"github.com/x/y\" // trailing y\n") {
		t.Errorf("the trailing comment goes with its spec:\n%s", got)
	}
}

func TestPruneKeepsGroupHeadingWhileGroupGoesOn(t *testing.T) {
	old := "package p\n\nimport (\n\t\"fmt\"\n\n\t// third-party\n\t\"github.com/x/y\"\n\t\"github.com/x/z\"\n)\n\nfunc F() { fmt.Println(); y.Do(); z.Do() }\n"
	got := pruneAfter(t, old, "y.Do(); ")
	if !strings.Contains(got, "\t// third-party\n\t\"github.com/x/z\"\n") {
		t.Errorf("heading of a group that goes on must stay:\n%s", got)
	}
}

func TestImportEditsKeepOrderOfMessyBlock(t *testing.T) {
	src := "package p\n\nimport (\n\t\"os\"\n\t\"fmt\"\n\t\"bytes\"\n)\n\nvar _ = os.Args\nvar _ = fmt.Sprint\nvar _ = bytes.MinRead\n"
	got, err := EditImports("p.go", src, "", []string{`"strings"`}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\t\"os\"\n\t\"fmt\"\n\t\"bytes\"\n\t\"strings\"\n)"; !strings.Contains(got, want) {
		t.Errorf("unsorted block: append, do not sort:\n%s", got)
	}
	got = pruneAfter(t, src, "var _ = fmt.Sprint\n")
	if !strings.Contains(got, "\t\"os\"\n\t\"bytes\"\n)") {
		t.Errorf("removal must not sort:\n%s", got)
	}
	sorted := "package p\n\nimport (\n\t\"bytes\"\n\t\"os\"\n)\n"
	got, _ = EditImports("p.go", sorted, "", []string{`"fmt"`}, false)
	if !strings.Contains(got, "\t\"bytes\"\n\t\"fmt\"\n\t\"os\"\n") {
		t.Errorf("sorted block: insert in place:\n%s", got)
	}
	got, _ = EditImports("p.go", sorted, "", []string{`"github.com/a/b"`}, false)
	if !strings.Contains(got, "\t\"os\"\n\n\t\"github.com/a/b\"\n)") {
		t.Errorf("a third-party import opens its own group:\n%s", got)
	}
}

func TestAdaptImportsReusesExistingSpec(t *testing.T) {
	dest := "package q\n\nimport \"strings\"\n\nvar _ = strings.ToLower\n"
	srcText := "package p\n\nimport str \"strings\"\n\nfunc F(s string) string { return str.ToUpper(s) }\n"
	snip, specs := AdaptImports("q.go", dest, "p.go", srcText, "func F(s string) string { return str.ToUpper(s) }")
	if snip != "func F(s string) string { return strings.ToUpper(s) }" || len(specs) != 1 || specs[0] != `"strings"` {
		t.Errorf("got %q %v", snip, specs)
	}
}
