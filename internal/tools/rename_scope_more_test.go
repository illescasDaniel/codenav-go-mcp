package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// REN-003: an interface method spec of another interface (fs.FileInfo.Name) has no receiver; its label is the type.
func TestForeignImplementerLabelsInterfaceSpec(t *testing.T) {
	src := "package fs\n\ntype FileInfo interface {\n\tName() string\n}\n"
	dir := writeModule(t, map[string]string{"fs/fs.go": src})
	path := filepath.Join(dir, "fs", "fs.go")
	if got := foreignImplementer(member{path, 3, 1}); got != "fs.FileInfo" {
		t.Errorf("foreignImplementer(interface spec) = %q, want fs.FileInfo", got)
	}
}

// REN-003: a position that names no type is labelled by its package, never by a path.
func TestForeignImplementerNeverLabelsAPath(t *testing.T) {
	src := "package fs\n\nvar X = 1\n"
	dir := writeModule(t, map[string]string{"fs/fs.go": src})
	path := filepath.Join(dir, "fs", "fs.go")
	got := foreignImplementer(member{path, 2, 4})
	if strings.Contains(got, "/") || strings.Contains(got, ".go") {
		t.Errorf("foreignImplementer fell back to a path: %q", got)
	}
}

// REN-004: a local or parameter named like an import (sub) is not that package: its edits are kept, while the
// package's own sub.B is dropped.
func TestDropForeignQualifiedKeepsLocalShadowingImport(t *testing.T) {
	src := "package p\n\nimport \"example.com/t/sub\"\n\nvar _ sub.B\n\nfunc f(sub struct{ A int }) int { return sub.A }\n"
	dir := writeModule(t, map[string]string{
		"p/p.go":     src,
		"sub/sub.go": "package sub\n\ntype B struct{ N int }\n",
	})
	w := &Workspace{Root: dir}
	path := filepath.Join(dir, "p", "p.go")
	by := map[string][]lsp.TextEdit{path: append(editsOf(src, "B", 4), editsOf(src, "A", 6)...)}
	dropped := dropForeignQualified(w, member{path, 0, 0}, by)
	if len(dropped) != 1 || !strings.Contains(dropped[0], "sub.B") {
		t.Fatalf("dropped %v, want only sub.B", dropped)
	}
	for _, e := range by[path] {
		if e.Range.Start.Line == 4 {
			t.Errorf("the package's sub.B edit was kept")
		}
	}
	kept := 0
	for _, e := range by[path] {
		if e.Range.Start.Line == 6 {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("kept %d edits on the parameter's line, want both A edits of the local", kept)
	}
}
