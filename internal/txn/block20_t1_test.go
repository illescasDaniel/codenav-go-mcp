package txn

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// longName returns a file name of n bytes ending in ".go".
func longName(n int) string {
	return strings.Repeat("n", n-len(".go")) + ".go"
}

// SCALE-006: a file whose name is near the 255-byte limit can be written; the temp file gets a short name.
func TestT1LongFileNameIsWritten(t *testing.T) {
	for _, n := range []int{240, 250, 255} {
		root := t.TempDir()
		a := filepath.Join(root, longName(n))
		write(t, a, "one\n")
		j := NewJournal(WriteGuard{Root: root})
		j.LogDir = t.TempDir()
		p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")})
		if _, err := j.Apply(p, false, "ENV"); err != nil {
			t.Fatalf("%d-byte name: %v", n, err)
		}
		if got := read(t, a); got != "two\n" {
			t.Fatalf("%d-byte name: content %q", n, got)
		}
		if _, err := j.Undo("", "ENV"); err != nil {
			t.Fatalf("%d-byte name: undo: %v", n, err)
		}
		if got := read(t, a); got != "one\n" {
			t.Fatalf("%d-byte name: undo left %q", n, got)
		}
	}
}

// SCALE-006: the temp name keeps the shape leftoverTemps recognises while it fits, and is short when it does not.
func TestT1TempNameFitsAnyLegalName(t *testing.T) {
	id := "0a1b2c3d"
	if got := tempName("c.go", id); got != ".c.go.0a1b2c3d.tmp" {
		t.Errorf("short name: got %q, want the block 18 shape", got)
	}
	for _, n := range []int{240, 241, 250, 255} {
		got := tempName(longName(n), id)
		if len(got) > maxNameLen {
			t.Errorf("%d-byte name: temp name %d bytes, over %d", n, len(got), maxNameLen)
		}
		if !strings.HasPrefix(got, ".") || !strings.HasSuffix(got, ".tmp") {
			t.Errorf("%d-byte name: temp name %q is not a hidden .tmp file", n, got)
		}
	}
}

// SCALE-006: a failed write answers with the cause only, not the path and temp name, and as a refusal.
func TestT1WriteFailureMessageIsShort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "locked")
	a := filepath.Join(dir, "a.go")
	write(t, a, "one\n")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")})
	_, err := j.Apply(p, false, "ENV")
	if err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	var ie *toolerr.InputError
	if !errors.As(err, &ie) {
		t.Fatalf("failure is not an InputError (isError would be false): %T %v", err, err)
	}
	if strings.Contains(err.Error(), ".tmp") || strings.Contains(err.Error(), root) {
		t.Errorf("failure message names the temp file or the path: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("failure message does not give the cause: %q", err.Error())
	}
}

// SCALE-006 crash recovery: a short-form temp file left beside a long-named file is named in the notice, with a
// removal line; the same name beside a file that would use the long form is not ours and is not offered.
func TestT1LeftoverNamesShortFormTempFile(t *testing.T) {
	root := t.TempDir()
	long := filepath.Join(root, longName(250))
	write(t, long, "one\n")
	short := filepath.Join(root, "c.go")
	write(t, short, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	p := edits.NewPlan("edit long name", []edits.FileChange{change(t, long, "one\n", "two\n")})
	l, note := beginIntent(j.LogDir, "apply", p)
	if l == nil {
		t.Fatal(note)
	}
	tmp := filepath.Join(root, ".codenav-0a1b2c3d.tmp")
	write(t, tmp, "two\n")
	other := filepath.Join(root, ".codenav-0a1b2c3d.tmp.x") // not the shape
	write(t, other, "x\n")
	l.in.PID = 0
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	var got []string
	j.Notify = func(m string) { got = append(got, m) }
	j.CheckPending()
	if len(got) != 1 || !strings.Contains(got[0], tmp) || !strings.Contains(got[0], "rm -- "+tmp) {
		t.Fatalf("notice does not name the short-form temp file:\n%v", got)
	}
	if strings.Contains(got[0], other) {
		t.Errorf("a file that is not a write temp file was named:\n%v", got)
	}
	// Beside a short base name the short form cannot be ours.
	if list := leftoverTemps(short); len(list) != 0 {
		t.Errorf("short-named file offered a short-form temp file: %v", list)
	}
}

// UNDO-001: an unknown preview id says that previews and undo history are kept in memory, and what to use instead.
func TestT1UnknownPreviewSaysMemoryOnly(t *testing.T) {
	s := NewPlanStore()
	_, err := s.Get("deadbeef")
	if err == nil || !strings.Contains(err.Error(), "live in memory in this server process") ||
		!strings.Contains(err.Error(), "use undo_edit with an applied edit id") {
		t.Errorf("unknown preview id: %v", err)
	}
}

// UNDO-001: undo refusals for ids and for an empty history say the history is in memory.
func TestT1UndoRefusalsSayMemoryOnly(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: root})
	j.LogDir = t.TempDir()
	if _, err := j.Undo("", "ENV"); err == nil || !strings.Contains(err.Error(), memoryNote) {
		t.Errorf("empty history: %v", err)
	}
	if _, err := j.Undo("abcd1234", "ENV"); err == nil || !strings.Contains(err.Error(), memoryNote) {
		t.Errorf("unknown id, empty history: %v", err)
	}
	if _, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Undo("abcd1234", "ENV"); err == nil || !strings.Contains(err.Error(), memoryNote) {
		t.Errorf("unknown id, with history: %v", err)
	}
}

// UNDO-002: a preview dropped by a workspace switch says so, not "unknown or expired".
func TestT1PreviewDroppedByWorkspaceSwitch(t *testing.T) {
	s := NewPlanStore()
	id := s.Add(edits.NewPlan("edit", nil))
	s.Clear("workspace switch to /work/new")
	_, err := s.Get(id)
	if err == nil || !strings.Contains(err.Error(), "dropped by the workspace switch to /work/new") ||
		strings.Contains(err.Error(), "unknown or expired") {
		t.Errorf("dropped preview: %v", err)
	}
}

// UNDO-002: an applied edit dropped by a workspace switch is reported as dropped by it.
func TestT1UndoAfterWorkspaceSwitchSaysDropped(t *testing.T) {
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	a := filepath.Join(oldRoot, "a.go")
	write(t, a, "one\n")
	j := NewJournal(WriteGuard{Root: oldRoot})
	j.LogDir = t.TempDir()
	e, err := j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")}), false, "ENV")
	if err != nil {
		t.Fatal(err)
	}
	j.Rebind(WriteGuard{Root: newRoot})
	_, err = j.Undo(e.ID, "ENV")
	want := "dropped by the workspace switch to " + newRoot
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("undo after switch: %v, want it to contain %q", err, want)
	}
	if read(t, a) != "two\n" {
		t.Error("the edit in the old workspace was touched")
	}
}

// The dropped-id memory is capped: the newest maxDropped ids keep their reason, the oldest are forgotten.
func TestT1DroppedIdsAreCapped(t *testing.T) {
	m, order := map[string]string{}, []string{}
	var ids []string
	for i := range maxDropped + 50 {
		id := "id" + strconv.Itoa(i)
		ids = append(ids, id)
		remember(m, &order, id, "workspace switch to /x")
	}
	if len(m) != maxDropped || len(order) != maxDropped {
		t.Fatalf("dropped ids kept: %d (order %d), want %d", len(m), len(order), maxDropped)
	}
	if _, ok := m[ids[0]]; ok {
		t.Errorf("oldest id still remembered")
	}
	if m[ids[len(ids)-1]] != "workspace switch to /x" {
		t.Errorf("newest id not remembered")
	}
	// The store's Clear feeds the same memory.
	s := NewPlanStore()
	pending := s.Add(edits.NewPlan("edit", nil))
	s.Clear("workspace switch to /y")
	if _, err := s.Get(pending); err == nil || !strings.Contains(err.Error(), "dropped by the workspace switch to /y") {
		t.Errorf("cleared preview not remembered: %v", err)
	}
}

// The file-limit refusal text names the id and the allow_large call; CheckPlan itself stores nothing.
func TestT1LargeRefusalText(t *testing.T) {
	root := t.TempDir()
	g := WriteGuard{Root: root}
	p := largePlan(root, DefaultMaxFiles+1)
	if !g.OverLimit(p, false) || g.OverLimit(p, true) {
		t.Fatal("OverLimit does not follow allowLarge")
	}
	msg := LargeRefusal(len(p.Changes), g.FileLimit(), "1a2b3c4d")
	for _, want := range []string{"touches 41 files (limit 40)", "Preview id 1a2b3c4d", `apply_edit(id="1a2b3c4d", allow_large=true)`} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q: %s", want, msg)
		}
	}
	if err := g.CheckPlan(p, false); err == nil || !strings.Contains(err.Error(), "allow_large=true") {
		t.Errorf("CheckPlan over the limit: %v", err)
	}
}

// largePlan is a plan that creates n new files under root.
func largePlan(root string, n int) *edits.EditPlan {
	var cs []edits.FileChange
	for i := range n {
		s := "package p\n"
		cs = append(cs, edits.FileChange{Path: filepath.Join(root, "f", "f"+strconv.Itoa(i)+".go"), New: &s})
	}
	return edits.NewPlan("many files", cs)
}
