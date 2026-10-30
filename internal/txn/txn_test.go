package txn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

func change(t *testing.T, path, old, next string) edits.FileChange {
	t.Helper()
	c := edits.FileChange{Path: path, New: &next}
	if old != "" {
		c.Old = &old
	}
	return c
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGuardPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	g := WriteGuard{Root: root, NeverWrite: []string{filepath.Join(root, "mod")}}
	cases := map[string]bool{ // path -> allowed
		filepath.Join(root, "a", "b.go"):      true,
		filepath.Join(root, "vendor", "x.go"): false,
		filepath.Join(root, ".git", "x.go"):   false,
		filepath.Join(root, "mod", "x.go"):    false,
		filepath.Join(root, "link", "x.go"):   false,
		filepath.Join(outside, "x.go"):        false,
		filepath.Join(root, "..", "x.go"):     false,
		filepath.Join(root, "new", "deep.go"): true,
		filepath.Join(root, "vendored.go"):    true,
	}
	for p, ok := range cases {
		if err := g.CheckPath(p); (err == nil) != ok {
			t.Errorf("%s: allowed=%v, err=%v", p, ok, err)
		}
	}
}

func TestGuardPlanLimits(t *testing.T) {
	root := t.TempDir()
	g := WriteGuard{Root: root, MaxFiles: 2}
	var cs []edits.FileChange
	for _, n := range []string{"a", "b", "c"} {
		cs = append(cs, change(t, filepath.Join(root, n+".go"), "", "package p\n"))
	}
	p := edits.NewPlan("x", cs)
	if err := g.CheckPlan(p, false); err == nil || !strings.Contains(err.Error(), "allow_large") {
		t.Errorf("want a size refusal, got %v", err)
	}
	if err := g.CheckPlan(p, true); err != nil {
		t.Error(err)
	}
	g.Extra = func(edits.FileChange) error { return os.ErrInvalid }
	if g.CheckPlan(p, true) == nil {
		t.Error("Extra rule not applied")
	}
	if (WriteGuard{ReadOnly: true}).CheckWritesEnabled("ENV") == nil {
		t.Error("read-only guard must refuse")
	}
}

func TestApplyUndo(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "sub", "b.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n"), change(t, b, "", "new\n")})
	e, err := j.Apply(p, false, "ENV")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "two\n" || read(t, b) != "new\n" {
		t.Fatal("changes not written")
	}
	if _, err := j.Undo(e.ID, "ENV"); err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "one\n" {
		t.Error("a.go not restored")
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Error("created file not removed")
	}
	if _, err := os.Stat(filepath.Dir(b)); !os.IsNotExist(err) {
		t.Error("empty directory left behind")
	}
	if _, err := j.Undo("", "ENV"); err == nil {
		t.Error("second undo must fail")
	}
}

func TestUndoRefusesStale(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	write(t, a, "someone else\n")
	if _, err := j.Undo("", "ENV"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("want stale refusal, got %v", err)
	}
	if read(t, a) != "someone else\n" {
		t.Error("stale undo must not touch the file")
	}
}

func TestApplyRefusesStalePlanAndReadOnly(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")})
	write(t, a, "drifted\n")
	if _, err := j.Apply(p, false, "ENV"); err == nil {
		t.Error("a plan made against other content must not apply")
	}
	write(t, a, "one\n")
	j.Guard.ReadOnly = true
	if _, err := j.Apply(p, false, "ENV"); err == nil {
		t.Error("read-only must refuse")
	}
	if read(t, a) != "one\n" {
		t.Error("nothing may be written")
	}
}

func TestApplyRollsBack(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	blocker := filepath.Join(root, "file")
	write(t, a, "one\n")
	write(t, blocker, "x") // a file where the second change needs a directory
	j := NewJournal(WriteGuard{Root: root})
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n"), change(t, filepath.Join(blocker, "c.go"), "", "new\n")})
	if _, err := j.Apply(p, false, "ENV"); err == nil {
		t.Fatal("want a write failure")
	}
	if read(t, a) != "one\n" {
		t.Error("first change must be rolled back")
	}
}

func TestPlanStore(t *testing.T) {
	s := NewPlanStore()
	id := s.Add(edits.NewPlan("x", nil))
	if _, err := s.Get(id); err != nil {
		t.Error(err)
	}
	s.Discard(id)
	if _, err := s.Get(id); err == nil {
		t.Error("discarded plan still found")
	}
}
