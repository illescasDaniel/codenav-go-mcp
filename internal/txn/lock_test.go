package txn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

func shortLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := lockWait
	lockWait = d
	t.Cleanup(func() { lockWait = old })
}

func TestSecondJournalWaitsForTheLock(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "one\n")
	j1, j2 := NewJournal(WriteGuard{Root: root}), NewJournal(WriteGuard{Root: root})
	unlock, _, err := j1.lockWrites()
	if err != nil {
		t.Fatal(err)
	}

	shortLockWait(t, 100*time.Millisecond)
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "one\n", "two\n")})
	if _, err := j2.Apply(p, false, "ENV"); err == nil || !strings.Contains(err.Error(), "another codenav-go-mcp process") {
		t.Fatalf("a held lock must stop the second writer with a clear message, got %v", err)
	}
	if read(t, a) != "one\n" {
		t.Fatal("nothing may be written while the lock is held")
	}

	shortLockWait(t, 10*time.Second)
	done := make(chan error, 1)
	go func() { _, err := j2.Apply(p, false, "ENV"); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("Apply must wait for the lock, returned %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if read(t, a) != "two\n" {
		t.Error("the waiting writer must write once the lock is free")
	}
}

func TestConcurrentJournalsOnOneFileWriteOnce(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	write(t, a, "base\n")
	beforeWriteHook = func(string) { time.Sleep(60 * time.Millisecond) }
	t.Cleanup(func() { beforeWriteHook = nil })
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, text := range []string{"from first\n", "from second\n"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := NewJournal(WriteGuard{Root: root})
			_, errs[i] = j.Apply(edits.NewPlan("edit", []edits.FileChange{change(t, a, "base\n", text)}), false, "ENV")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !strings.Contains(err.Error(), "changed since the preview") {
			t.Errorf("the loser must be told the file changed, got %v", err)
		}
	}
	if got := read(t, a); ok != 1 || (got != "from first\n" && got != "from second\n") {
		t.Errorf("exactly one writer must win, got %d winners and %q", ok, got)
	}
}

func TestApplyNoticesAChangeRightBeforeTheWrite(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	write(t, a, "a1\n")
	write(t, b, "b1\n")
	beforeWriteHook = func(path string) {
		if path == b {
			write(t, b, "someone else\n")
		}
	}
	t.Cleanup(func() { beforeWriteHook = nil })
	j := NewJournal(WriteGuard{Root: root})
	p := edits.NewPlan("edit", []edits.FileChange{change(t, a, "a1\n", "a2\n"), change(t, b, "b1\n", "b2\n")})
	_, err := j.Apply(p, false, "ENV")
	if err == nil || !strings.Contains(err.Error(), "changed on disk just before it was written") || !strings.Contains(err.Error(), "b.go") {
		t.Fatalf("want a clear message naming b.go, got %v", err)
	}
	if read(t, a) != "a1\n" || read(t, b) != "someone else\n" {
		t.Errorf("a.go must be rolled back and the other writer's b.go kept: %q %q", read(t, a), read(t, b))
	}
	if _, err := os.Stat(filepath.Join(j.LogDir, intentFile)); err == nil {
		t.Error("the intent log must be cleared after the rollback")
	}
}
