//go:build unix

package txn

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// WRX-003: a file created by a write gets the mode the umask allows, as os.WriteFile would give it.
func TestNewFilesHonourUmask(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "old.go")
	write(t, existing, "one\n")
	if err := os.Chmod(existing, 0o644); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	j := NewJournal(WriteGuard{Root: root})
	created := filepath.Join(root, "sub", "new.go")
	s := "package sub\n"
	if _, err := j.Apply(edits.NewPlan("create", []edits.FileChange{{Path: created, New: &s}}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(created); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("created file mode: want 0600, got %v (%v)", st.Mode().Perm(), err)
	}
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, existing, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(existing); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("existing file mode must be kept: want 0644, got %v (%v)", st.Mode().Perm(), err)
	}
	// Under a group-writable umask the new file is 0644 less the umask, not 0664 (os.WriteFile's result).
	syscall.Umask(0o002)
	loose := filepath.Join(root, "sub", "loose.go")
	if _, err := j.Apply(edits.NewPlan("create", []edits.FileChange{{Path: loose, New: &s}}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(loose); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("created file under umask 002: want 0644, got %v (%v)", st.Mode().Perm(), err)
	}
}

// UNDO-003: undoing a deletion restores the file with the mode it had (the exec bit included).
func TestUndoRestoresDeletedFileMode(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "pm", "m.go")
	write(t, a, "package pm\n")
	if err := os.Chmod(a, 0o755); err != nil {
		t.Fatal(err)
	}
	j := NewJournal(WriteGuard{Root: root})
	old := read(t, a)
	if _, err := j.Apply(edits.NewPlan("delete", []edits.FileChange{{Path: a, Old: &old}}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Undo("", "ENV"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(a)
	if err != nil || st.Mode().Perm() != 0o755 || read(t, a) != old {
		t.Errorf("undo of a deletion: want mode 0755 and the same bytes, got %v (%v)", st.Mode().Perm(), err)
	}
}

// EDIT-007: a write that gives a hard-linked file a new inode says so; the other names keep the old content.
func TestHardLinkNoteNamesTheOtherNames(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "hl_a.go"), filepath.Join(root, "_hl_b.go")
	write(t, a, "return 1\n")
	if err := os.Link(a, b); err != nil {
		t.Fatal(err)
	}
	notes := HardLinkNotes(edits.NewPlan("edit", []edits.FileChange{change(t, a, "return 1\n", "return 2\n")}), root)
	if len(notes) != 1 || !strings.Contains(notes[0], "2 hard links") {
		t.Errorf("want a hard-link note, got %v", notes)
	}
	lone := filepath.Join(root, "lone.go")
	write(t, lone, "return 1\n")
	if notes := HardLinkNotes(edits.NewPlan("edit", []edits.FileChange{change(t, lone, "return 1\n", "return 2\n")}), root); len(notes) != 0 {
		t.Errorf("a file with one name gets no note, got %v", notes)
	}
}
