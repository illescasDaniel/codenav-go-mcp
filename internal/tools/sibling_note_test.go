package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-DIAG-001: the diagnostics note names a sibling file of the same build that does not parse, and only then.
func TestSiblingSyntaxNoteNamesTheBrokenSibling(t *testing.T) {
	root := t.TempDir()
	put := func(rel, src string) string {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const clean = "package d2\n\nfunc Use() int { return Helper() + 1 }\n"
	const broken = "package d2\n\nfunc Broken( {\n\treturn\n}\n\nfunc Helper() int { return 1 }\n"
	clean2 := put("d2/clean.go", clean)
	put("d2/broken.go", broken)

	const want = "(note: package has a syntax error in d2/broken.go:3:14;"
	if got := siblingSyntaxNote(root, clean2, clean); !strings.HasPrefix(got, want) {
		t.Errorf("broken sibling: got %q, want prefix %q", got, want)
	}
	// A file that does not parse is reported by gopls itself: no note about its siblings.
	if got := siblingSyntaxNote(root, filepath.Join(root, "d2/broken.go"), broken); got != "" {
		t.Errorf("broken target: got %q, want no note", got)
	}

	// A sibling excluded from this build (ignored by its //go:build line) does not break the package.
	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root2, "d3"), 0o755); err != nil {
		t.Fatal(err)
	}
	ign := filepath.Join(root2, "d3", "gen.go")
	if err := os.WriteFile(ign, []byte("//go:build ignore\n\npackage main\n\nfunc main( {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c3 := filepath.Join(root2, "d3", "c.go")
	if err := os.WriteFile(c3, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := siblingSyntaxNote(root2, c3, clean); got != "" {
		t.Errorf("excluded sibling: got %q, want no note", got)
	}
}

// A broken _test.go sibling is in the test build only, so a non-test file gets no note for it, but a test file does.
func TestSiblingSyntaxNoteTestFilesOnlyForTests(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "d4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const clean = "package d4\n\nfunc Use() int { return 1 }\n"
	const test = "package d4\n\nimport \"testing\"\n\nfunc TestX(t *testing.T {\n}\n"
	for name, src := range map[string]string{"c.go": clean, "c_test.go": test, "t_test.go": clean} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := siblingSyntaxNote(root, filepath.Join(dir, "c.go"), clean); got != "" {
		t.Errorf("non-test file, broken test sibling: got %q, want no note", got)
	}
	if got := siblingSyntaxNote(root, filepath.Join(dir, "t_test.go"), clean); !strings.Contains(got, "d4/c_test.go:5:") {
		t.Errorf("test file, broken test sibling: got %q, want d4/c_test.go", got)
	}
}
