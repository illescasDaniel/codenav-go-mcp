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

func TestPlanStoreAppliedID(t *testing.T) {
	s := NewPlanStore()
	id := s.Add(edits.NewPlan("x", nil))
	s.MarkApplied(id, "e1")
	if _, err := s.Get(id); err == nil || !strings.Contains(err.Error(), "already applied") || !strings.Contains(err.Error(), `undo_edit(id="e1")`) {
		t.Errorf("want an already-applied error, got %v", err)
	}
}

func TestCheckPathRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.go")
	write(t, target, "package p\n")
	link := filepath.Join(root, "link.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	g := WriteGuard{Root: root}
	if err := g.CheckPath(link); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("want symlink refusal, got %v", err)
	}
	if err := g.CheckPath(target); err != nil {
		t.Errorf("regular file refused: %v", err)
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "txn-state")
	if err != nil {
		panic(err)
	}
	os.Setenv(StateDirEnv, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestAppliedHashesAreOfWrittenBytes(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	afterWritesHook = func() { write(t, a, "someone else\n") }
	defer func() { afterWritesHook = nil }()
	e, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV")
	if err != nil {
		t.Fatal(err)
	}
	if e.AppliedHashes[a] != edits.HashBytes([]byte("two\n")) {
		t.Error("the hash must be of the bytes we wrote")
	}
	if _, err := j.Undo("", "ENV"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("a write that raced ours must block undo, got %v", err)
	}
}

func TestUndoReportsFailedRollback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	root := t.TempDir()
	a, b := filepath.Join(root, "x", "a.go"), filepath.Join(root, "y", "b.go")
	write(t, a, "a1\n")
	write(t, b, "b1\n")
	j := NewJournal(WriteGuard{Root: root})
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "a1\n", "a2\n"), change(t, b, "b1\n", "b2\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	// Undo writes b first, then a (whose directory is now read-only); the rollback of b then fails too.
	if err := os.Chmod(filepath.Dir(a), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(a), 0o755)
	restoreFn = func(edits.FileChange) error { return os.ErrPermission }
	defer func() { restoreFn = restore }()
	_, err := j.Undo("", "ENV")
	if err == nil || !strings.Contains(err.Error(), "COULD NOT RESTORE: y/b.go") || strings.Contains(err.Error(), "nothing was reverted") {
		t.Errorf("want the stuck file named, got %v", err)
	}
}

func TestIntentLogLeftoverAfterCrash(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	p := edits.NewPlan("edit things", []edits.FileChange{change(t, a, "one\n", "two\n"), change(t, b, "", "new\n")})
	// Simulate a crash after the first write: begin the log, write one file, never finish.
	l, note := beginIntent(j.LogDir, "apply", p)
	if l == nil {
		t.Fatal(note)
	}
	l.in.PID = 0 // the writer is gone
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	write(t, a, "two\n")
	var got []string
	j.Notify = func(m string) { got = append(got, m) }
	j.CheckPending()
	if len(got) != 1 || !strings.Contains(got[0], "a.go") || !strings.Contains(got[0], "already has the new content") ||
		!strings.Contains(got[0], "b.go") || !strings.Contains(got[0], "rm "+b) || !strings.Contains(got[0], "cp ") {
		t.Fatalf("notice does not describe the half-written files:\n%v", got)
	}
	orig, err := os.ReadFile(strings.Fields(strings.SplitN(got[0][strings.Index(got[0], "cp "):], "\n", 2)[0])[1])
	if err != nil || string(orig) != "one\n" {
		t.Errorf("stored original: %q, %v", orig, err)
	}
	j.CheckPending()
	if len(got) != 1 {
		t.Error("a leftover is reported once")
	}
}

func TestIntentLogClearedAfterApplyAndRollback(t *testing.T) {
	root := t.TempDir()
	a, blocker := filepath.Join(root, "a.go"), filepath.Join(root, "file")
	write(t, a, "one\n")
	write(t, blocker, "x")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	if _, err := j.Apply(edits.NewPlan("ok", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	bad := edits.NewPlan("bad", []edits.FileChange{change(t, a, "two\n", "three\n"), change(t, filepath.Join(blocker, "c.go"), "", "new\n")})
	if _, err := j.Apply(bad, false, "ENV"); err == nil {
		t.Fatal("want failure")
	}
	if _, err := os.Stat(filepath.Join(j.LogDir, intentFile)); !os.IsNotExist(err) {
		t.Error("the log must be cleared after success and after a clean rollback")
	}
}

func TestIntentLogFailureDoesNotBlockEdit(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	notFile := filepath.Join(t.TempDir(), "f")
	write(t, notFile, "x")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = filepath.Join(notFile, "sub") // cannot be created
	var got []string
	j.Notify = func(m string) { got = append(got, m) }
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "two\n" || len(got) != 1 || !strings.Contains(got[0], "not logged") {
		t.Errorf("edit must go through with a note, got %v", got)
	}
}

func TestRebindSaysHistoryWasDropped(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	var got []string
	j.Notify = func(m string) { got = append(got, m) }
	j.Rebind(WriteGuard{Root: t.TempDir()})
	if len(got) != 1 || !strings.Contains(got[0], "undo history of 1 edit") {
		t.Errorf("got %v", got)
	}
}
