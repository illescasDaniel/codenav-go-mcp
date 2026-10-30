package tools

import (
	"os"
	"path/filepath"
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
