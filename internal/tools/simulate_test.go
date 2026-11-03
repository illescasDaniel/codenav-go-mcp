package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

func TestPackageStubKeepsOnlyPackageClause(t *testing.T) {
	if got := packageStub("// doc\npackage shapes // c\n\nfunc F() {}\n"); got != "package shapes\n" {
		t.Errorf("got %q", got)
	}
	if got := packageStub("not go"); got != "" {
		t.Errorf("unparsable source must give no stub, got %q", got)
	}
}

func TestDirFullyDeleted(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	del := func(n string) edits.FileChange {
		return edits.FileChange{Path: filepath.Join(dir, n), Old: edits.Str("package p\n")}
	}
	both := &edits.EditPlan{Changes: []edits.FileChange{del("a.go"), del("b.go")}}
	set := map[string]bool{filepath.Join(dir, "a.go"): true, filepath.Join(dir, "b.go"): true}
	if !dirFullyDeleted(dir, both, set) {
		t.Error("all Go files deleted")
	}
	one := &edits.EditPlan{Changes: []edits.FileChange{del("a.go")}}
	if dirFullyDeleted(dir, one, map[string]bool{filepath.Join(dir, "a.go"): true}) {
		t.Error("b.go stays")
	}
	made := &edits.EditPlan{Changes: append([]edits.FileChange{{Path: filepath.Join(dir, "n.go"), New: edits.Str("package p\n")}}, both.Changes...)}
	if dirFullyDeleted(dir, made, set) {
		t.Error("a file is created in the directory")
	}
}

// A body-only edit changes no top-level shape; a new signature, a type, or a typed var does. Methods have none.
func TestTopDeclShapesIgnoreBodies(t *testing.T) {
	src := "package p\n\nfunc F(n int) int { return n }\n\ntype T struct{ A int }\n\nvar V int = 1\n\nfunc (T) M() {}\n"
	base := topDeclShapes(src)
	if got := topDeclShapes(strings.Replace(src, "return n }", "return n + 1 }", 1)); got["F"] != base["F"] {
		t.Errorf("a body-only edit changed the shape of F: %q", got["F"])
	}
	if got := topDeclShapes(strings.Replace(src, "F(n int) int", "F(n, m int) int", 1)); got["F"] == base["F"] {
		t.Errorf("a new signature must change the shape of F")
	}
	if got := topDeclShapes(strings.Replace(src, "A int", "A int64", 1)); got["T"] == base["T"] {
		t.Errorf("a changed field must change the shape of T")
	}
	if got := topDeclShapes(strings.Replace(src, "var V int = 1", "var V int64 = 1", 1)); got["V"] == base["V"] {
		t.Errorf("a changed type must change the shape of V")
	}
	if _, ok := base["M"]; ok {
		t.Errorf("methods are not top-level names: %v", base)
	}
}
