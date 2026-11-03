package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// writeModule writes a go.mod (module example.com/t) and the files under a fresh directory.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/t\n\ngo 1.21\n"
	for name, text := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// editsOf is a rename's edits in file: every whole-word occurrence of word on the given 0-based lines.
func editsOf(text, word string, lines ...int) []lsp.TextEdit {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`)
	var out []lsp.TextEdit
	for _, ln := range lines {
		for _, loc := range re.FindAllStringIndex(strings.Split(text, "\n")[ln], -1) {
			out = append(out, lsp.TextEdit{
				Range:   lsp.Range{Start: lsp.Position{Line: ln, Character: loc[0]}, End: lsp.Position{Line: ln, Character: loc[1]}},
				NewText: "new",
			})
		}
	}
	return out
}

// REN-001: a receiver is renamed inside its own method only; the other methods' receivers are other objects.
func TestLimitToReceiverKeepsOneMethod(t *testing.T) {
	src := "package p\n\ntype Server struct{ port int }\n\nfunc (s *Server) Start() { s.port = 1 }\n\nfunc (s *Server) Port() int { return s.port }\n"
	dir := writeModule(t, map[string]string{"p/p.go": src})
	path := filepath.Join(dir, "p", "p.go")
	// The receiver of Start is on 0-based line 4, column 6 (the s of "func (s").
	decl := member{path, 4, 6}
	by := map[string][]lsp.TextEdit{path: editsOf(src, "s", 4, 6)}
	if n := limitToReceiver(decl, by); n != 2 {
		t.Errorf("dropped %d edits, want the two of Port", n)
	}
	for _, e := range by[path] {
		if e.Range.Start.Line != 4 {
			t.Errorf("an edit outside Start was kept: line %d", e.Range.Start.Line+1)
		}
	}
	if len(by[path]) != 2 {
		t.Errorf("kept %d edits, want the receiver and its use in Start", len(by[path]))
	}
	// A name that is not a receiver (the field port, line 2) is left alone.
	other := map[string][]lsp.TextEdit{path: editsOf(src, "port", 2)}
	if n := limitToReceiver(member{path, 2, 20}, other); n != 0 || len(other[path]) != 1 {
		t.Errorf("a field is not a receiver: dropped %d, kept %d", n, len(other[path]))
	}
}

// REN-004 / BED-003: an identifier another package qualifies is not the renamed symbol.
func TestDropForeignQualifiedKeepsOwnPackage(t *testing.T) {
	src := "package p\n\nimport \"example.com/t/sub\"\n\n// This is [cmp.Ordered] and [Square.Area].\ntype A = sub.B\n\ntype S struct{ A }\n"
	dir := writeModule(t, map[string]string{
		"p/p.go":     src,
		"sub/sub.go": "package sub\n\ntype B struct{ N int }\n",
	})
	w := &Workspace{Root: dir}
	path := filepath.Join(dir, "p", "p.go")
	decl := member{path, 5, 5} // A in "type A = sub.B"
	// The edits a gopls rename of A would make: sub.B (the alias' target, another package), A itself, the
	// field A, and two doc links: [cmp.Ordered] (another package) and [Square.Area] (a type of this package).
	by := map[string][]lsp.TextEdit{path: append(editsOf(src, "B", 5), editsOf(src, "A", 5, 7)...)}
	by[path] = append(by[path], editsOf(src, "Ordered", 4)...)
	by[path] = append(by[path], editsOf(src, "Area", 4)...)

	dropped := dropForeignQualified(w, decl, by)
	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want sub.B and cmp.Ordered", dropped)
	}
	joined := strings.Join(dropped, " ")
	if !strings.Contains(joined, "sub.B") || !strings.Contains(joined, "cmp.Ordered") {
		t.Errorf("dropped labels: %v", dropped)
	}
	// [Square.Area] names a type of this package (not a package), and the A edits stay.
	kept := by[path]
	if len(kept) != 3 {
		t.Errorf("kept %d edits, want the two A edits and Square.Area's Area", len(kept))
	}
}

// The identifier of a qualifier: "sub" in sub.B, whichever part the edit spans.
func TestQualifierOf(t *testing.T) {
	text := "x := sub.B + [cmp.Ordered]"
	if q, from, ok := qualifierOf(text, 9, 10); !ok || q != "sub" || from != 5 {
		t.Errorf("sub.B by its name: %q %d %v", q, from, ok)
	}
	if q, from, ok := qualifierOf(text, 5, 10); !ok || q != "sub" || from != 5 {
		t.Errorf("sub.B spanned: %q %d %v", q, from, ok)
	}
	if _, _, ok := qualifierOf(text, 0, 1); ok {
		t.Error("an unqualified name has no qualifier")
	}
}

// REN-003: the note names the types outside the workspace that satisfy the interface, and is empty without them.
func TestSkippedNameTheForeignImplementers(t *testing.T) {
	if got := skippedNote(nil, "Equal"); got != "" {
		t.Errorf("no foreign implementers, no note: %q", got)
	}
	got := skippedNote([]string{"assert.Assertions", "assert.Assertions", "bytes.Buffer"}, "Equal")
	if !strings.Contains(got, "assert.Assertions") || !strings.Contains(got, "2 type(s)") || strings.Contains(got, "unaffected") {
		t.Errorf("skippedNote: %q", got)
	}
}

func TestForeignImplementerLabelsTheType(t *testing.T) {
	src := "package assert\n\ntype Assertions struct{}\n\nfunc (a *Assertions) Equal(expected, actual any) bool { return true }\n"
	dir := writeModule(t, map[string]string{"assert/assert.go": src})
	path := filepath.Join(dir, "assert", "assert.go")
	if got := foreignImplementer(member{path, 4, 21}); got != "assert.Assertions" {
		t.Errorf("foreignImplementer = %q, want assert.Assertions", got)
	}
}

// BED-003 (the doc link to a standard-library package the file does not import) on its own.
func TestDropForeignQualifiedDocLinkToStdlib(t *testing.T) {
	src := "package constraints\n\n// This type is redundant since Go 1.21 introduced [cmp.Ordered].\ntype Ordered interface{ ~int }\n"
	dir := writeModule(t, map[string]string{"constraints/constraints.go": src})
	w := &Workspace{Root: dir}
	path := filepath.Join(dir, "constraints", "constraints.go")
	by := map[string][]lsp.TextEdit{path: editsOf(src, "Ordered", 2, 3)}
	dropped := dropForeignQualified(w, member{path, 3, 5}, by)
	if len(dropped) != 1 || !strings.Contains(dropped[0], "cmp.Ordered") {
		t.Fatalf("dropped %v, want the cmp.Ordered link", dropped)
	}
	if len(by[path]) != 1 {
		t.Errorf("kept %d edits, want the declaration only", len(by[path]))
	}
}
