package mentions

import (
	"os"
	"path/filepath"
	"testing"
)

// SCALE-003: a leftover scan that reaches its file cap reports it, and the files past the cap are not read.
func TestBlock20EScanCapIsReported(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("the Widget is here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := maxFiles
	t.Cleanup(func() { maxFiles = old })
	maxFiles = 2
	found, truncated := FindScopedLimited(dir, "Widget", nil, "", nil)
	if !truncated {
		t.Errorf("three files against a cap of two must report the cap")
	}
	if len(found) != 2 {
		t.Errorf("found %d mentions, want the two files inside the cap", len(found))
	}
	if ScanCap() != 2 {
		t.Errorf("ScanCap() = %d, want the shrunken cap 2", ScanCap())
	}
	maxFiles = 3
	if _, truncated := FindScopedLimited(dir, "Widget", nil, "", nil); truncated {
		t.Errorf("three files within a cap of three must not report the cap")
	}
}
