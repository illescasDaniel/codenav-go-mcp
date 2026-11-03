package txn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// EDIT-005: a read-only file is refused before any write, whatever the caller says.
func TestCheckPlanRefusesReadOnlyFile(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "ro.go")
	write(t, a, "return 1\n")
	if err := os.Chmod(a, 0o444); err != nil {
		t.Fatal(err)
	}
	j := NewJournal(WriteGuard{Root: root})
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "return 1\n", "return 2\n")})
	if err := j.Guard.CheckPlan(p, false); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("CheckPlan: want a read-only refusal, got %v", err)
	}
	if _, err := j.Apply(p, true, "ENV"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("Apply: want a read-only refusal, got %v", err)
	}
	if got := read(t, a); got != "return 1\n" {
		t.Errorf("a read-only file was written: %q", got)
	}
	old := "return 1\n"
	del := edits.NewPlan("delete", []edits.FileChange{{Path: a, Old: &old}})
	if _, err := j.Apply(del, true, "ENV"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("delete: want a read-only refusal, got %v", err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Errorf("a read-only file was deleted: %v", err)
	}
}

// UNDO-004: undo removes only the directories the edit itself created.
func TestUndoKeepsDirectoryTheEditDidNotCreate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "newdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	j := NewJournal(WriteGuard{Root: root})
	s := "package newdir\n"
	if _, err := j.Apply(edits.NewPlan("create", []edits.FileChange{{Path: filepath.Join(dir, "x.go"), New: &s}}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Undo("", "ENV"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("undo removed the directory the user made: %v", err)
	}
	// The directory the edit does create is still removed by undo.
	made := filepath.Join(root, "made", "deep")
	if _, err := j.Apply(edits.NewPlan("create", []edits.FileChange{{Path: filepath.Join(made, "y.go"), New: &s}}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Undo("", "ENV"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "made")); !os.IsNotExist(err) {
		t.Errorf("undo left the directory the edit created: %v", err)
	}
}

// WRX-001: a temp file a kill left beside an interrupted write is named in the notice, with a removal line.
func TestLeftoverNamesTempFileOfInterruptedWrite(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "c.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	p := edits.NewPlan("edit things", []edits.FileChange{change(t, a, "one\n", "two\n")})
	l, note := beginIntent(j.LogDir, "apply", p)
	if l == nil {
		t.Fatal(note)
	}
	tmp := filepath.Join(root, ".c.go.0a1b2c3d.tmp")
	write(t, tmp, "two\n")
	other := filepath.Join(root, "c.go.0a1b2c3d.tmp") // not ours: no leading dot
	write(t, other, "x\n")
	l.in.PID = 0 // the writer is gone
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	var got []string
	j.Notify = func(m string) { got = append(got, m) }
	j.CheckPending()
	if len(got) != 1 || !strings.Contains(got[0], tmp) || !strings.Contains(got[0], "rm -- "+tmp) {
		t.Fatalf("notice does not name the temp file:\n%v", got)
	}
	if strings.Contains(got[0], other) {
		t.Errorf("a file that is not a write temp file was named:\n%v", got)
	}
}
